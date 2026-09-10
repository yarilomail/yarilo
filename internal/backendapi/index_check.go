package backendapi

import (
	"net/http"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/idxrebuild"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

type indexCheckRequest struct {
	User      string `json:"user"`
	Namespace string `json:"namespace"`
	Fix       bool   `json:"fix"`
}

type indexCheckFolder struct {
	Folder   string `json:"folder"`
	Checked  int    `json:"checked"`
	Shifted  int    `json:"shifted"`
	Repaired int    `json:"repaired"`
}

type indexCheckStats struct {
	User       string             `json:"user"`
	Checked    int                `json:"checked"`
	Shifted    int                `json:"shifted"`
	Repaired   int                `json:"repaired"`
	Folders    []indexCheckFolder `json:"folders,omitempty"`
	Failed     map[string]string  `json:"failed,omitempty"`
	DurationMs int64              `json:"duration_ms"`
	Note       string             `json:"note"`
}

// handleIndexCheck reads every folder of one account looking for records whose
// tail was written at a width the base did not announce (#1770), and with
// fix=true rebuilds each one from the message in storage.
func (s *Server) handleIndexCheck(w http.ResponseWriter, r *http.Request) {
	var req indexCheckRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	uc, err := s.openUserContext(req.User)
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
	entries, err := bundle.box.ListFolders()
	if err != nil {
		apiError(w, "list folders: "+err.Error(), http.StatusInternalServerError)
		return
	}
	stored, err := idxrebuild.StoredTails(bundle.mbox)
	if err != nil {
		apiError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	start := time.Now()
	out := indexCheckStats{User: req.User, Note: indexCheckNote(req.Fix, stored == nil)}
	for _, e := range entries {
		folder, oerr := bundle.idx.OpenFolder(e.Name, 0)
		if oerr != nil {
			if out.Failed == nil {
				out.Failed = map[string]string{}
			}
			out.Failed[e.Name] = oerr.Error()
			continue
		}
		st, rerr := folderTailStats(bundle.mbox, folder, stored, req.Fix)
		if rerr != nil {
			if out.Failed == nil {
				out.Failed = map[string]string{}
			}
			out.Failed[e.Name] = rerr.Error()
			continue
		}
		out.Checked += st.Checked
		out.Shifted += st.Shifted
		out.Repaired += st.Repaired
		if st.Shifted > 0 {
			out.Folders = append(out.Folders, indexCheckFolder{
				Folder: folder.Name, Checked: st.Checked, Shifted: st.Shifted, Repaired: st.Repaired,
			})
		}
	}
	out.DurationMs = time.Since(start).Milliseconds()
	apiJSON(w, out)
}

// folderTailStats counts without fix and repairs with it, so an operator can
// read the account before changing a byte in it.
func folderTailStats(b mailbox.Box, folder *mailbox.Folder, stored map[uint32]mailbox.ScanRecord, fix bool) (idxrebuild.TailStats, error) {
	if fix {
		return idxrebuild.RepairShiftedTails(b, folder, stored)
	}
	var st idxrebuild.TailStats
	msgs, err := b.Index().GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		return st, err
	}
	st.Checked = len(msgs)
	for _, m := range msgs {
		if idxrebuild.ShiftedTail(m) {
			st.Shifted++
		}
	}
	return st, nil
}

func indexCheckNote(fix, noStorageKeys bool) string {
	if noStorageKeys {
		return "this driver keeps no storage key per record, so no record can carry a shifted tail"
	}
	if fix {
		return "each repaired record's tail (storage key, size, guid) was rebuilt from the message in storage; run with the user's mailboxes quiesced"
	}
	return "read-only: rerun with --fix to rebuild every shifted record's tail from storage"
}
