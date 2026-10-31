package backendapi

import (
	"fmt"
	"net/http"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

type guidRebuildRequest struct {
	User      string `json:"user"`
	Namespace string `json:"namespace"`
}

type guidRebuildStats struct {
	Folders int `json:"folders"`
	Copies  int `json:"copies"`
	// Complete is false when a folder missed a command meanwhile: run it again.
	Complete bool `json:"complete"`
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
	var ids []uint64
	for _, e := range entries {
		if !e.Selectable {
			continue
		}
		folder, ferr := bundle.mbox.Folder(e.Name, 0)
		if ferr != nil {
			return nil, http.StatusInternalServerError, fmt.Errorf("open folder %q: %w", e.Name, ferr)
		}
		ids = append(ids, folder.ID)
	}
	copies, err := rebuilder.RebuildGUIDStore(ids)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("write the store: %w", err)
	}
	stats := &guidRebuildStats{Folders: len(ids), Copies: copies}
	// A delivery that gave up waiting for the store marked its folder: the
	// answer says so, and the operator runs the rebuild again.
	if res, ok := bundle.idx.(mailbox.GUIDResolver); ok {
		stats.Complete, _ = res.GUIDStoreComplete(ids)
	}
	return stats, http.StatusOK, nil
}
