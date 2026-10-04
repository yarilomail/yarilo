package backendapi

import (
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// registerFolderRoutes registers folder admin routes. Reads live
// here; mutating ops are in folder_write.go. Mutating ops bypass
// ACL (admin plane is gated by token/AllowedNets/mTLS), but ACL
// files are still moved on rename and dropped on delete.
func (s *Server) registerFolderRoutes() {
	s.mux.Handle("POST /api/backend/folder/list", s.middleware(s.handleFolderList))
	s.mux.Handle("POST /api/backend/folder/info", s.middleware(s.handleFolderInfo))
	s.mux.Handle("POST /api/backend/folder/guid", s.middleware(s.handleFolderGUID))
	s.mux.Handle("POST /api/backend/folder/stats", s.middleware(s.handleFolderStats))
	s.mux.Handle("POST /api/backend/folder/repair", s.middleware(s.handleFolderRepair))
	s.registerFolderWriteRoutes()
}

type folderRequest struct {
	User      string `json:"user"`
	Folder    string `json:"folder"`
	Namespace string `json:"namespace"`
}

type folderInfoOut struct {
	Name string `json:"name"`
	// Created is false for a configured mailbox no server has opened yet: a
	// diagnostic shows it and makes nothing (#1774, #2005).
	Created       bool   `json:"created"`
	GUID          string `json:"guid"`
	UIDValidity   uint32 `json:"uid_validity"`
	NextUID       uint32 `json:"next_uid"`
	Messages      uint32 `json:"messages"`
	Unseen        uint32 `json:"unseen"`
	HighestModSeq uint64 `json:"highest_modseq"`
}

func (s *Server) handleFolderList(w http.ResponseWriter, r *http.Request) {
	var req folderRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	uc, err := s.openUserContextReadOnly(req.User)
	if err != nil {
		apiError(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer uc.Close()

	bundle, err := uc.ns(s, req.Namespace)
	if err != nil {
		apiError(w, err.Error(), http.StatusBadRequest)
		return
	}
	var names []string
	var spec config.NamespaceConfig
	if bundle != nil {
		entries, err := bundle.box.ListFolders()
		if err != nil {
			apiError(w, "list folders: "+err.Error(), http.StatusInternalServerError)
			return
		}
		names, spec = mailbox.SelectableNames(entries), bundle.spec
	} else if spec, err = s.listedNamespace(req.Namespace); err != nil {
		// No mail home and nothing configured: the account has nothing yet.
		apiError(w, errNoMailHome.Error(), http.StatusNotFound)
		return
	}
	// Configured mailboxes are listed as present, as a listing of the
	// reference does, and nothing is made: this is a read (#1774).
	pending := s.configuredMissing(spec, names)
	names = append(names, pending...)
	sort.Strings(names)
	apiJSON(w, map[string]any{"folders": names, "not_created": pending, "special_use": s.listedSpecialUse(spec, names)})
}

// listedNamespace is the configuration of a namespace whose store does not
// exist yet; an error when it configures no mailboxes to show.
func (s *Server) listedNamespace(name string) (config.NamespaceConfig, error) {
	if name == "" {
		name = "personal"
	}
	spec, ok := s.namespaceByName(name)
	if !ok || len(spec.AutoMailboxes()) == 0 {
		return config.NamespaceConfig{}, errNoMailHome
	}
	return spec, nil
}

// configuredMissing are the namespace's auto mailboxes not in names.
func (s *Server) configuredMissing(spec config.NamespaceConfig, names []string) []string {
	have := make(map[string]bool, len(names))
	for _, n := range names {
		have[n] = true
	}
	out := []string{}
	for name, mb := range spec.AutoMailboxes() {
		if mb.Auto != mailbox.AutoNo && !have[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// listedSpecialUse is the folded special-use map over the listed names; the
// map belongs to the personal namespace alone.
func (s *Server) listedSpecialUse(spec config.NamespaceConfig, names []string) map[string]string {
	out := map[string]string{}
	if !strings.EqualFold(spec.Type, "personal") || s.ownsStore(spec) {
		return out
	}
	for _, n := range names {
		if attr, ok := s.opts.SpecialUseDefaults[n]; ok {
			out[n] = attr
		}
	}
	return out
}

func (s *Server) handleFolderInfo(w http.ResponseWriter, r *http.Request) {
	out, status, err := s.folderInfoCommon(w, r)
	if errors.Is(err, errFolderNotFound) && out != nil {
		// A configured mailbox not made yet: said so, with no identity made up.
		apiJSON(w, map[string]any{"name": out.Name, "created": false, "configured": true})
		return
	}
	if err != nil {
		apiError(w, err.Error(), status)
		return
	}
	apiJSON(w, out)
}

func (s *Server) handleFolderGUID(w http.ResponseWriter, r *http.Request) {
	out, status, err := s.folderInfoCommon(w, r)
	if err != nil {
		apiError(w, err.Error(), status)
		return
	}
	apiJSON(w, map[string]any{"folder": out.Name, "guid": out.GUID})
}

func (s *Server) handleFolderStats(w http.ResponseWriter, r *http.Request) {
	var req folderRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Folder == "" {
		apiError(w, "folder required", http.StatusBadRequest)
		return
	}
	uc, err := s.openUserContextReadOnly(req.User)
	if err != nil {
		apiError(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer uc.Close()

	bundle, ok := readBundle(w, s, uc, req.Namespace)
	if !ok {
		return
	}
	req.Folder = mailbox.NormalizeName(req.Folder, bundle.info.SkipNFCNormalize)
	exists, err := bundle.box.FolderExists(req.Folder)
	if err != nil {
		apiError(w, "folder exists: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !exists {
		apiError(w, "folder not found", http.StatusNotFound)
		return
	}
	folder, err := bundle.mbox.Folder(req.Folder, 0)
	if err != nil {
		apiError(w, "open folder: "+err.Error(), http.StatusInternalServerError)
		return
	}
	out := map[string]any{
		"name":           folder.Name,
		"guid":           hex.EncodeToString(folder.GUID[:]),
		"uid_validity":   folder.UIDValidity,
		"next_uid":       folder.NextUID,
		"messages":       folder.Messages,
		"unseen":         folder.Unseen,
		"highest_modseq": folder.HighestModSeq,
		// Null rather than zero where the driver cannot answer: a folder-
		// agnostic store keeps its messages user-wide, so it has no per-folder
		// listing to count or measure. Reporting 0 made "not applicable" look
		// like a fact, which breaks any comparison between drivers and fires
		// every "this folder is empty on disk" alert forever (#1224).
		"size_bytes":    nil,
		"on_disk_count": nil,
	}
	if !isFolderAgnostic(bundle.box) {
		msgs, err := bundle.box.List(req.Folder)
		if err != nil {
			apiError(w, "list messages: "+err.Error(), http.StatusInternalServerError)
			return
		}
		var sizeBytes uint64
		for _, m := range msgs {
			sizeBytes += uint64(m.Size)
		}
		out["size_bytes"] = sizeBytes
		out["on_disk_count"] = uint32(len(msgs))
	}
	apiJSON(w, out)
}

// isFolderAgnostic reports whether the driver keeps its messages outside any
// one folder, in which case a per-folder count of files on disk is not a
// number it has.
func isFolderAgnostic(box mailbox.UserMailbox) bool {
	fa, ok := box.(mailbox.FolderAgnosticStorage)
	return ok && fa.FolderAgnosticScan()
}

// folderInfoCommon opens the folder and returns its metadata.
func (s *Server) folderInfoCommon(w http.ResponseWriter, r *http.Request) (*folderInfoOut, int, error) {
	var req folderRequest
	if !decodeJSON(w, r, &req) {
		return nil, 0, errDecode
	}
	if req.Folder == "" {
		return nil, http.StatusBadRequest, errFolderRequired
	}
	uc, err := s.openUserContextReadOnly(req.User)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	defer uc.Close()

	bundle, err := uc.ns(s, req.Namespace)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if bundle == nil {
		return nil, http.StatusNotFound, errNoMailHome
	}
	req.Folder = mailbox.NormalizeName(req.Folder, bundle.info.SkipNFCNormalize)
	exists, err := bundle.box.FolderExists(req.Folder)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if !exists {
		if mb, ok := bundle.spec.AutoMailboxes()[req.Folder]; ok && mb.Auto != mailbox.AutoNo {
			return &folderInfoOut{Name: req.Folder}, http.StatusNotFound, errFolderNotFound
		}
		return nil, http.StatusNotFound, errFolderNotFound
	}
	folder, err := bundle.mbox.Folder(req.Folder, 0)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return &folderInfoOut{
		Name:          folder.Name,
		Created:       true,
		GUID:          hex.EncodeToString(folder.GUID[:]),
		UIDValidity:   folder.UIDValidity,
		NextUID:       folder.NextUID,
		Messages:      folder.Messages,
		Unseen:        folder.Unseen,
		HighestModSeq: folder.HighestModSeq,
	}, http.StatusOK, nil
}
