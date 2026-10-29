package jmap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-message"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailops"
	"github.com/yarilomail/yarilo/v2/pkg/jmapcore"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

type importRequest struct {
	AccountID string                 `json:"accountId"`
	IfInState *string                `json:"ifInState"`
	Emails    map[string]importEmail `json:"emails"`
}

type importEmail struct {
	BlobID     string          `json:"blobId"`
	MailboxIDs map[string]bool `json:"mailboxIds"`
	Keywords   map[string]bool `json:"keywords"`
	ReceivedAt *time.Time      `json:"receivedAt"`
}

type importResponse struct {
	AccountID  string                        `json:"accountId"`
	OldState   string                        `json:"oldState"`
	NewState   string                        `json:"newState"`
	Created    map[string]map[string]any     `json:"created"`
	NotCreated map[string]*jmapcore.SetError `json:"notCreated"`
}

// emailImport stores blobs as messages (RFC 8621 §4.8) through the mutation
// layer: quota, the index and the events fire as for a delivery.
func (s *Server) emailImport(ctx context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req importRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	for cid, e := range req.Emails {
		e.MailboxIDs = jmapcore.ResolveKeys(ctx, e.MailboxIDs)
		req.Emails[cid] = e
	}
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	if max := s.opts.Limits.MaxObjectsInSet; max > 0 && len(req.Emails) > max {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrRequestTooLarge}
	}
	state, serr := s.emailState(h)
	if serr != nil {
		return nil, storeFailure("Email/import state", accountID, serr)
	}
	if req.IfInState != nil && *req.IfInState != state {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrStateMismatch}
	}
	resp := &importResponse{
		AccountID: accountID, OldState: state, NewState: state,
		Created: map[string]map[string]any{}, NotCreated: map[string]*jmapcore.SetError{},
	}
	ids := make([]string, 0, len(req.Emails))
	for id := range req.Emails {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, cid := range ids {
		created, setErr := s.importOne(ctx, h, req.Emails[cid])
		if setErr != nil {
			resp.NotCreated[cid] = setErr
			continue
		}
		resp.Created[cid] = created
		noteCreated(ctx, cid, created)
	}
	if len(resp.Created) > 0 {
		if after, err := s.emailState(h); err == nil {
			resp.NewState = after
		}
	}
	return resp, nil
}

type importTarget struct {
	name   string
	folder *mailbox.Folder
}

func (s *Server) importOne(ctx context.Context, h *userHandle, e importEmail) (map[string]any, *jmapcore.SetError) {
	targets, setErr := h.importTargets(e.MailboxIDs)
	if setErr != nil {
		return nil, setErr
	}
	raw, err := s.readBlob(ctx, h, e.BlobID)
	if err != nil {
		return nil, &jmapcore.SetError{Type: jmapcore.SetErrInvalidProps, Properties: []string{"blobId"}}
	}
	if entity, _ := message.Read(bytes.NewReader(raw)); entity == nil {
		return nil, &jmapcore.SetError{Type: jmapcore.SetErrInvalidProps, Properties: []string{"blobId"},
			Description: "the blob is not a message"}
	}
	keywords := map[string]bool{}
	for k, v := range e.Keywords {
		if v {
			keywords[strings.ToLower(k)] = true
		}
	}
	flags, custom := splitKeywords(keywords)
	received := time.Now()
	if e.ReceivedAt != nil {
		received = *e.ReceivedAt
	}
	return h.storeEmail(raw, targets, flags, custom, received)
}

// storeEmail saves raw into every target as one Email: the first save names
// it, the rest keep its GUID (RFC 8474). It answers the created Email.
func (h *userHandle) storeEmail(raw []byte, targets []importTarget, flags, custom []string, received time.Time) (map[string]any, *jmapcore.SetError) {
	var guid [16]byte
	var size int64
	for _, t := range targets {
		saved, err := h.ops.Save(h.mbox, t.name, t.folder, mailops.SaveRequest{
			Body: bytes.NewReader(raw), Size: int64(len(raw)), Flags: flags, Keywords: custom,
			Received: received, GUID: guid,
		})
		var refused *mailops.RefusedError
		switch {
		case errors.As(err, &refused):
			return nil, &jmapcore.SetError{Type: jmapcore.SetErrOverQuota, Description: refused.Error()}
		case err != nil:
			return nil, &jmapcore.SetError{Type: jmapcore.SetErrForbidden, Description: err.Error()}
		}
		guid, size = saved.Meta.GUID, int64(saved.Meta.Size)
	}
	id := mailbox.FormatObjectID(guid)
	return map[string]any{"id": id, "blobId": id, "threadId": h.threadOf(id), "size": size}, nil
}

// importTargets resolves mailboxIds through the caller's own folders; an
// unknown or absent one names nothing (#712 account boundary).
func (h *userHandle) importTargets(ids map[string]bool) ([]importTarget, *jmapcore.SetError) {
	bad := &jmapcore.SetError{Type: jmapcore.SetErrInvalidProps, Properties: []string{"mailboxIds"}}
	var keys []string
	for id, on := range ids {
		if on {
			keys = append(keys, id)
		}
	}
	if len(keys) == 0 {
		return nil, bad
	}
	sort.Strings(keys)
	out := make([]importTarget, 0, len(keys))
	for _, id := range keys {
		guid, err := parseObjectID(id)
		if err != nil {
			return nil, bad
		}
		name, f, err := h.folderByGUID(guid)
		if err != nil || f == nil {
			return nil, bad
		}
		out = append(out, importTarget{name: name, folder: f})
	}
	return out, nil
}

// readBlob reads an uploaded blob, a message or one of its parts, all from
// the caller's own account.
func (s *Server) readBlob(ctx context.Context, h *userHandle, blobID string) ([]byte, error) {
	if up, ok := parseUploadID(blobID, s.uploadMax()); ok {
		st, err := h.uploads(s.opts.Storage.Locker, s.opts.UploadTTL)
		if err != nil {
			return nil, err
		}
		f, err := st.open(ctx, up)
		if err != nil {
			return nil, err
		}
		defer f.Close() //nolint:errcheck
		return io.ReadAll(f)
	}
	msgID, partID, isPart := parsePartBlobID(blobID)
	if !isPart {
		msgID = blobID
	}
	found, err := s.findMessages(h, map[string]bool{msgID: true})
	if err != nil {
		return nil, err
	}
	ref, ok := found[msgID]
	if !ok {
		return nil, errNoBlob
	}
	rc, err := h.mbox.OpenMessage(ref.folder, ref.meta)
	if err != nil {
		return nil, errNoBlob
	}
	defer rc.Close() //nolint:errcheck
	if !isPart {
		return io.ReadAll(rc)
	}
	entity, _ := message.Read(rc)
	if entity == nil {
		return nil, errNoBlob
	}
	body, ok := partBody(entity, partID)
	if !ok {
		return nil, errNoBlob
	}
	return []byte(body), nil
}
