package backendapi

import (
	"fmt"
	"net/http"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

type guidRebuildRequest struct {
	User      string `json:"user"`
	Namespace string `json:"namespace"`
}

type guidRebuildStats struct {
	Folders int `json:"folders"`
	Copies  int `json:"copies"`
}

// handleGUIDRebuild writes the per-user GUID store from the folder indexes:
// the store is derived, and this is what repairs it (#1711).
func (s *Server) handleGUIDRebuild(w http.ResponseWriter, r *http.Request) {
	var req guidRebuildRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	stats, status, err := s.rebuildGUIDStore(req)
	if err != nil {
		apiError(w, err.Error(), status)
		return
	}
	apiJSON(w, stats)
}

func (s *Server) rebuildGUIDStore(req guidRebuildRequest) (*guidRebuildStats, int, error) {
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
	rebuilder, ok := bundle.idx.(mailbox.GUIDStoreRebuilder)
	if !ok {
		return nil, http.StatusNotImplemented, fmt.Errorf("this index keeps no GUID store")
	}

	entries, err := bundle.box.ListFolders()
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("list folders: %w", err)
	}
	stats := &guidRebuildStats{}
	var copies []mailbox.GUIDRecord
	for _, e := range entries {
		if !e.Selectable {
			continue
		}
		folder, ferr := bundle.mbox.Folder(e.Name, 0)
		if ferr != nil {
			return nil, http.StatusInternalServerError, fmt.Errorf("open folder %q: %w", e.Name, ferr)
		}
		metas, merr := bundle.mbox.Messages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
		if merr != nil {
			return nil, http.StatusInternalServerError, fmt.Errorf("read folder %q: %w", e.Name, merr)
		}
		stats.Folders++
		for _, m := range metas {
			if m.GUID == ([16]byte{}) {
				// A record with no GUID names no id: the backfill that stamps
				// them is its own command.
				continue
			}
			copies = append(copies, mailbox.GUIDRecord{
				GUID: m.GUID, FolderGUID: folder.GUID, UID: m.UID,
				InternalDate: m.InternalDate.Unix(),
			})
		}
	}
	if err := rebuilder.ReplaceGUIDStore(copies); err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("write the store: %w", err)
	}
	stats.Copies = len(copies)
	return stats, http.StatusOK, nil
}
