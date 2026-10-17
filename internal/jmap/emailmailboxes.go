package jmap

import (
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"

	"github.com/yarilomail/yarilo/internal/storage/mailops"
	"github.com/yarilomail/yarilo/pkg/jmapcore"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// mailboxPlan is where one update puts the Email: the whole set, or patches
// that add or remove one mailbox each (RFC 8620 §5.3).
type mailboxPlan struct {
	replace     map[string]bool
	add, remove map[string]bool
}

func badMailboxIDs(desc string) *jmapcore.SetError {
	return &jmapcore.SetError{Type: jmapcore.SetErrInvalidProps, Properties: []string{"mailboxIds"}, Description: desc}
}

// readMailboxPatch adds one "mailboxIds" or "mailboxIds/<id>" member to plan.
func readMailboxPatch(plan *mailboxPlan, name string, raw json.RawMessage) *jmapcore.SetError {
	if name == "mailboxIds" {
		set := map[string]bool{}
		if err := json.Unmarshal(raw, &set); err != nil {
			return badMailboxIDs(err.Error())
		}
		plan.replace = map[string]bool{}
		for id, on := range set {
			if on {
				plan.replace[id] = true
			}
		}
		return nil
	}
	id := strings.TrimPrefix(name, "mailboxIds/")
	var on *bool
	if id == "" || json.Unmarshal(raw, &on) != nil {
		return badMailboxIDs("a mailboxIds patch names one mailbox and true or null")
	}
	if on != nil && *on {
		plan.add[id] = true
	} else {
		plan.remove[id] = true
	}
	return nil
}

type folderTarget struct {
	name   string
	folder *mailbox.Folder
}

// applyMailboxes puts the Email in exactly the mailboxes plan names: a copy
// into each new one keeping its GUID, then an expunge from each it leaves.
func (s *Server) applyMailboxes(h *userHandle, ref messageRef, plan *mailboxPlan) *jmapcore.SetError {
	if plan.replace != nil && (len(plan.add) > 0 || len(plan.remove) > 0) {
		return &jmapcore.SetError{Type: jmapcore.SetErrInvalidPatch, Properties: []string{"mailboxIds"},
			Description: "mailboxIds was given both directly and as a patch"}
	}
	copies := h.copiesOf(ref)
	sort.Slice(copies, func(i, j int) bool { return copies[i].folder < copies[j].folder })
	have := map[string][]emailCopy{}
	for _, c := range copies {
		have[c.mailboxID] = append(have[c.mailboxID], c)
	}
	want := plan.replace
	if want == nil {
		want = map[string]bool{}
		for id := range have {
			want[id] = !plan.remove[id]
		}
		for id := range plan.add {
			want[id] = true
		}
	}
	var adds []folderTarget
	stays := 0
	for id, on := range want {
		if !on {
			continue
		}
		if _, held := have[id]; held {
			stays++
			continue
		}
		t, ok := h.targetFolder(id)
		if !ok {
			return badMailboxIDs("no such mailbox: " + id)
		}
		adds = append(adds, t)
	}
	if stays+len(adds) == 0 {
		return badMailboxIDs("an Email is in at least one mailbox")
	}
	var leaves []string
	for id := range have {
		if !want[id] {
			leaves = append(leaves, id)
		}
	}
	sort.Slice(adds, func(i, j int) bool { return adds[i].name < adds[j].name })
	sort.Slice(leaves, func(i, j int) bool { return have[leaves[i]][0].folder < have[leaves[j]][0].folder })

	if len(adds) > 0 {
		src := copies[0]
		raw, err := readCopy(h, src)
		if err != nil {
			return &jmapcore.SetError{Type: jmapcore.SetErrNotFound}
		}
		// A move does not grow the account: the reference nets the source out.
		netGrowth := len(adds) > len(leaves)
		for _, t := range adds {
			_, err := h.ops.Save(h.mbox, t.name, t.folder, mailops.SaveRequest{
				Body: strings.NewReader(string(raw)), Size: int64(len(raw)),
				Flags: src.meta.Flags, Keywords: src.meta.Keywords,
				Received: src.meta.InternalDate, GUID: src.meta.GUID, QuotaChecked: !netGrowth,
			})
			var refused *mailops.RefusedError
			if errors.As(err, &refused) {
				return &jmapcore.SetError{Type: jmapcore.SetErrOverQuota, Description: refused.Error()}
			}
			if err != nil {
				return &jmapcore.SetError{Type: jmapcore.SetErrForbidden, Description: err.Error()}
			}
		}
	}
	for _, id := range leaves {
		if serr := h.expungeCopies(have[id]); serr != nil {
			return serr
		}
	}
	return nil
}

// targetFolder resolves a mailbox id through the caller's own folders only.
func (h *userHandle) targetFolder(id string) (folderTarget, bool) {
	guid, err := parseObjectID(id)
	if err != nil {
		return folderTarget{}, false
	}
	name, f, err := h.folderByGUID(guid)
	if err != nil || f == nil {
		return folderTarget{}, false
	}
	return folderTarget{name: name, folder: f}, true
}

func readCopy(h *userHandle, c emailCopy) ([]byte, error) {
	rc, err := h.mbox.OpenMessage(c.folder, c.meta)
	if err != nil {
		return nil, err
	}
	defer rc.Close() //nolint:errcheck
	return io.ReadAll(rc)
}

// expungeCopies removes the copies of one folder through the mutation layer.
func (h *userHandle) expungeCopies(copies []emailCopy) *jmapcore.SetError {
	if len(copies) == 0 {
		return nil
	}
	f, err := h.mbox.Folder(copies[0].folder, 0)
	if err != nil {
		return &jmapcore.SetError{Type: jmapcore.SetErrForbidden, Description: err.Error()}
	}
	metas := make([]*mailbox.MessageMeta, 0, len(copies))
	for _, c := range copies {
		metas = append(metas, c.meta)
	}
	if _, _, err := h.ops.ExpungeMarked(h.mbox, f, copies[0].folder, metas); err != nil {
		return &jmapcore.SetError{Type: jmapcore.SetErrForbidden, Description: err.Error()}
	}
	return nil
}

// emailDestroy expunges every copy of each Email, folder by folder in name order.
func (s *Server) emailDestroy(h *userHandle, ids []string, resp *jmapcore.SetResponse) error {
	if len(ids) == 0 {
		return nil
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	found, err := s.findMessages(h, want)
	if err != nil {
		return err
	}
	for _, id := range ids {
		ref, ok := found[id]
		if !ok {
			resp.NotDestroyed[id] = &jmapcore.SetError{Type: jmapcore.SetErrNotFound}
			continue
		}
		byFolder := map[string][]emailCopy{}
		var names []string
		for _, c := range h.copiesOf(ref) {
			if _, seen := byFolder[c.folder]; !seen {
				names = append(names, c.folder)
			}
			byFolder[c.folder] = append(byFolder[c.folder], c)
		}
		sort.Strings(names)
		var failed *jmapcore.SetError
		for _, name := range names {
			if serr := h.expungeCopies(byFolder[name]); serr != nil {
				failed = serr
				break
			}
		}
		if failed != nil {
			resp.NotDestroyed[id] = failed
			continue
		}
		resp.Destroyed = append(resp.Destroyed, id)
	}
	return nil
}
