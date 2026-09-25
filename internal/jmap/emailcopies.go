package jmap

import (
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// emailCopy is one mailbox that holds the message, with the record that
// mailbox keeps of it: keywords are per copy, the Email's are their union.
type emailCopy struct {
	mailboxID string
	folder    string
	folderID  uint64
	meta      *mailbox.MessageMeta
}

// copiesOf lists every mailbox holding the message. The store answers it in
// one read (#1711); without one the folders are walked, as id lookup does.
func (h *userHandle) copiesOf(ref messageRef) []emailCopy {
	if out, ok := h.copiesThroughStore(ref); ok {
		return out
	}
	return h.copiesByWalk(ref)
}

func (h *userHandle) copiesThroughStore(ref messageRef) ([]emailCopy, bool) {
	res, ok := h.idx.(mailbox.GUIDResolver)
	if !ok {
		return nil, false
	}
	records, err := res.GUIDCopies([][16]byte{ref.meta.GUID})
	if err != nil || len(records) == 0 {
		return nil, false
	}
	// Grouped by folder, and read by uid: a copy is one record, not a folder
	// listing, and this sits on the client's synchronisation path.
	byFolder := make(map[[16]byte][]uint32, 4)
	for _, r := range records {
		byFolder[r.FolderGUID] = append(byFolder[r.FolderGUID], r.UID)
	}
	out := make([]emailCopy, 0, len(records))
	for folderGUID, uids := range byFolder {
		name, f, ferr := h.folderByGUID(folderGUID)
		if ferr != nil || f == nil {
			return nil, false
		}
		set := make(mailbox.SeqSet, 0, len(uids))
		for _, uid := range uids {
			set = append(set, mailbox.SeqRange{From: uid, To: uid})
		}
		metas, merr := h.mbox.Messages(f.ID, set)
		if merr != nil || len(metas) != len(uids) {
			// The store is ahead of the folder: fall back rather than report
			// a mailbox that no longer holds the message.
			return nil, false
		}
		for _, m := range metas {
			out = append(out, emailCopy{mailboxID: mailboxID(f.GUID), folder: name, folderID: f.ID, meta: m})
		}
	}
	return out, true
}

func (h *userHandle) copiesByWalk(ref messageRef) []emailCopy {
	entries, err := h.box.ListFolders()
	if err != nil {
		return []emailCopy{{mailboxID: ref.mailboxID, folder: ref.folder, folderID: ref.folderID, meta: ref.meta}}
	}
	var out []emailCopy
	for _, name := range mailbox.SelectableNames(entries) {
		f, ferr := h.mbox.Folder(name, 0)
		if ferr != nil {
			continue
		}
		metas, merr := h.mbox.Messages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
		if merr != nil {
			continue
		}
		for _, m := range metas {
			if m.GUID == ref.meta.GUID {
				out = append(out, emailCopy{mailboxID: mailboxID(f.GUID), folder: name, folderID: f.ID, meta: m})
			}
		}
	}
	if len(out) == 0 {
		out = append(out, emailCopy{mailboxID: ref.mailboxID, folder: ref.folder, folderID: ref.folderID, meta: ref.meta})
	}
	return out
}

// mailboxIDsOf and keywordsAcross are what RFC 8621 §4 makes properties of one
// Email rather than of a copy: where it is, and the union of its keywords.
func mailboxIDsOf(copies []emailCopy) map[string]bool {
	out := make(map[string]bool, len(copies))
	for _, c := range copies {
		out[c.mailboxID] = true
	}
	return out
}

func keywordsAcross(copies []emailCopy) map[string]bool {
	out := map[string]bool{}
	for _, c := range copies {
		for k, v := range keywordsOf(c.meta) {
			if v {
				out[k] = true
			}
		}
	}
	return out
}
