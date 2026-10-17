package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/yarilomail/yarilo/internal/mailboxcreate"
	"github.com/yarilomail/yarilo/pkg/jmapcore"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

type mailboxSetRequest struct {
	jmapcore.SetRequest
	OnDestroyRemoveEmails bool `json:"onDestroyRemoveEmails"`
}

// mailboxProps is what Mailbox/set may write; role and sortOrder are read-only
// here, and any other property is refused by name.
type mailboxProps struct {
	Name         *string `json:"name"`
	ParentID     *string `json:"parentId"`
	IsSubscribed *bool   `json:"isSubscribed"`
}

func readMailboxProps(raw json.RawMessage) (mailboxProps, map[string]bool, *jmapcore.SetError) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return mailboxProps{}, nil, &jmapcore.SetError{Type: jmapcore.SetErrInvalidProps, Description: err.Error()}
	}
	var bad []string
	for k := range fields {
		switch k {
		case "name", "parentId", "isSubscribed":
		default:
			bad = append(bad, k)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return mailboxProps{}, nil, &jmapcore.SetError{Type: jmapcore.SetErrInvalidProps, Properties: bad}
	}
	var p mailboxProps
	if err := json.Unmarshal(raw, &p); err != nil {
		return mailboxProps{}, nil, &jmapcore.SetError{Type: jmapcore.SetErrInvalidProps, Description: err.Error()}
	}
	present := map[string]bool{}
	for k := range fields {
		present[k] = true
	}
	return p, present, nil
}

func invalidProp(name, desc string) *jmapcore.SetError {
	return &jmapcore.SetError{Type: jmapcore.SetErrInvalidProps, Properties: []string{name}, Description: desc}
}

// mailboxSet implements Mailbox/set (RFC 8621 §2.5) through the mutation layer.
func (s *Server) mailboxSet(ctx context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req mailboxSetRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	resolveSet(ctx, &req.SetRequest)
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	if max := s.opts.Limits.MaxObjectsInSet; max > 0 && len(req.Create)+len(req.Update)+len(req.Destroy) > max {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrRequestTooLarge}
	}
	before, err := s.mailboxList(h)
	if err != nil {
		return nil, storeFailure("Mailbox/set state", accountID, err)
	}
	state := mailboxState(before)
	if req.IfInState != nil && *req.IfInState != state {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrStateMismatch}
	}
	resp := jmapcore.NewSetResponse(accountID, state)
	sep := separator(h.info)

	order, cyclic := createOrder(req.Create)
	for _, cid := range cyclic {
		resp.NotCreated[cid] = invalidProp("parentId", "the parents named form a cycle")
	}
	for _, cid := range order {
		created, serr := s.mailboxCreate(h, resolveProperty(ctx, req.Create[cid], "parentId"), sep)
		if serr != nil {
			resp.NotCreated[cid] = serr
			continue
		}
		resp.Created[cid] = created
		noteCreated(ctx, cid, created)
	}
	for _, id := range sortedKeys(req.Update) {
		if serr := s.mailboxUpdate(h, id, resolveProperty(ctx, req.Update[id], "parentId"), sep); serr != nil {
			resp.NotUpdated[id] = serr
			continue
		}
		resp.Updated[id] = nil
	}
	for _, id := range req.Destroy {
		if serr := s.mailboxDestroy(h, id, sep, req.OnDestroyRemoveEmails); serr != nil {
			resp.NotDestroyed[id] = serr
			continue
		}
		resp.Destroyed = append(resp.Destroyed, id)
	}
	if len(resp.Created)+len(resp.Updated)+len(resp.Destroyed) > 0 {
		if after, err := s.mailboxList(h); err == nil {
			resp.NewState = mailboxState(after)
		}
	}
	return resp, nil
}

// createOrder puts a create after the create of the same call its parentId
// names as "#<creation id>" (RFC 8620 §5.3); the ones in a cycle are returned apart.
func createOrder(create map[string]json.RawMessage) (order, cyclic []string) {
	parent := map[string]string{}
	for cid, raw := range create {
		var p struct {
			ParentID *string `json:"parentId"`
		}
		if json.Unmarshal(raw, &p) == nil && p.ParentID != nil {
			if pc, ok := strings.CutPrefix(*p.ParentID, "#"); ok && create[pc] != nil {
				parent[cid] = pc
			}
		}
	}
	// A create in a cycle, or below one, is refused: its parent is never created.
	const visiting, done, refused = 1, 2, 3
	mark := map[string]int{}
	var visit func(cid string) bool
	visit = func(cid string) bool {
		switch mark[cid] {
		case visiting, refused:
			return false
		case done:
			return true
		}
		mark[cid] = visiting
		if pc, ok := parent[cid]; ok && !visit(pc) {
			mark[cid] = refused
			cyclic = append(cyclic, cid)
			return false
		}
		mark[cid] = done
		order = append(order, cid)
		return true
	}
	for _, cid := range sortedKeys(create) {
		visit(cid)
	}
	sort.Strings(cyclic)
	return order, cyclic
}

func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// parentName resolves a parentId to a folder name: a real folder, or a
// container node; null is the top level.
func (h *userHandle) parentName(id *string) (string, bool) {
	if id == nil {
		return "", true
	}
	if name, ok := strings.CutPrefix(*id, "container:"); ok {
		return name, name != ""
	}
	t, ok := h.targetFolder(*id)
	return t.name, ok
}

// checkNewName validates a full folder name as IMAP CREATE and RENAME do.
func (h *userHandle) checkNewName(full, leaf, sep string) *jmapcore.SetError {
	if leaf == "" || strings.Contains(leaf, sep) {
		return invalidProp("name", "a name is one level, not empty")
	}
	if exists, _ := h.box.FolderExists(full); exists {
		return &jmapcore.SetError{Type: jmapcore.SetErrAlreadyExists}
	}
	if err := mailbox.CheckName(h.box, full); err != nil {
		return invalidProp("name", err.Error())
	}
	return nil
}

func join(parent, leaf, sep string) string {
	if parent == "" {
		return leaf
	}
	return parent + sep + leaf
}

func (s *Server) mailboxCreate(h *userHandle, raw json.RawMessage, sep string) (any, *jmapcore.SetError) {
	p, _, serr := readMailboxProps(raw)
	if serr != nil {
		return nil, serr
	}
	if p.Name == nil {
		return nil, invalidProp("name", "name is required")
	}
	parent, ok := h.parentName(p.ParentID)
	if !ok {
		return nil, invalidProp("parentId", "no such mailbox")
	}
	full := join(parent, *p.Name, sep)
	if serr := h.checkNewName(full, *p.Name, sep); serr != nil {
		return nil, serr
	}
	if err := h.ops.CreateFolder(h.mbox, full, full); err != nil {
		if errors.Is(err, mailboxcreate.ErrLimit) {
			return nil, &jmapcore.SetError{Type: jmapcore.SetErrOverQuota, Description: "Maximum number of mailboxes reached"}
		}
		return nil, invalidProp("name", err.Error())
	}
	if p.IsSubscribed != nil && *p.IsSubscribed {
		h.setSubscribed(full, true)
	}
	f, err := h.mbox.Folder(full, 0)
	if err != nil {
		return nil, &jmapcore.SetError{Type: jmapcore.SetErrForbidden, Description: err.Error()}
	}
	h.folders.remember(f.GUID, full)
	return map[string]any{"id": mailboxID(f.GUID)}, nil
}

func (s *Server) mailboxUpdate(h *userHandle, id string, raw json.RawMessage, sep string) *jmapcore.SetError {
	t, ok := h.targetFolder(id)
	if !ok {
		return &jmapcore.SetError{Type: jmapcore.SetErrNotFound}
	}
	p, present, serr := readMailboxProps(raw)
	if serr != nil {
		return serr
	}
	if present["name"] || present["parentId"] {
		if strings.EqualFold(t.name, "INBOX") {
			return &jmapcore.SetError{Type: jmapcore.SetErrForbidden, Description: "INBOX is not renamed"}
		}
		parent := ""
		if i := strings.LastIndex(t.name, sep); i >= 0 {
			parent = t.name[:i]
		}
		if present["parentId"] {
			var ok bool
			if parent, ok = h.parentName(p.ParentID); !ok {
				return invalidProp("parentId", "no such mailbox")
			}
		}
		leaf := leafName(t.name, sep)
		if p.Name != nil {
			leaf = *p.Name
		}
		full := join(parent, leaf, sep)
		if full != t.name {
			if full == t.name+sep || strings.HasPrefix(full, t.name+sep) {
				return invalidProp("parentId", "a mailbox cannot move under itself")
			}
			if serr := h.checkNewName(full, leaf, sep); serr != nil {
				return serr
			}
			if err := h.ops.RenameFolder(h.mbox, h.acl, t.name, full, t.name, full); err != nil {
				return invalidProp("name", err.Error())
			}
			h.folders.remember(t.folder.GUID, full)
		}
	}
	if p.IsSubscribed != nil {
		name := t.name
		if n, ok := h.folders.name(t.folder.GUID); ok {
			name = n
		}
		h.setSubscribed(name, *p.IsSubscribed)
	}
	return nil
}

func (s *Server) mailboxDestroy(h *userHandle, id, sep string, removeEmails bool) *jmapcore.SetError {
	t, ok := h.targetFolder(id)
	if !ok {
		return &jmapcore.SetError{Type: jmapcore.SetErrNotFound}
	}
	if strings.EqualFold(t.name, "INBOX") {
		return &jmapcore.SetError{Type: jmapcore.SetErrForbidden, Description: "INBOX is not destroyed"}
	}
	entries, err := h.box.ListFolders()
	if err != nil {
		return &jmapcore.SetError{Type: jmapcore.SetErrForbidden, Description: err.Error()}
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name, t.name+sep) {
			return &jmapcore.SetError{Type: jmapcore.SetErrMailboxHasChild}
		}
	}
	if !removeEmails {
		if md, err := h.mbox.Metadata(t.folder.ID); err == nil && md.Messages > 0 {
			return &jmapcore.SetError{Type: jmapcore.SetErrMailboxHasEmail}
		}
	}
	if err := h.ops.DeleteFolder(h.mbox, h.acl, t.name, t.name); err != nil {
		return &jmapcore.SetError{Type: jmapcore.SetErrForbidden, Description: err.Error()}
	}
	h.folders.forget(t.folder.GUID)
	return nil
}

// setSubscribed writes the personal subscription file IMAP reads and announces it.
func (h *userHandle) setSubscribed(name string, on bool) {
	var err error
	if on {
		err = h.subs.Add(name)
	} else {
		err = h.subs.Remove(name)
	}
	if err == nil {
		h.ops.Subscribed(name, on)
	}
}
