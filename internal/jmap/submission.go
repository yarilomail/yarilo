package jmap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/mail"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	goSmtp "github.com/emersion/go-smtp"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Sender relays one message; *proxy.Submission, the relay SMTP Submission uses.
type Sender interface {
	Send(from string, rcpts []string, body io.Reader, clientIP net.IP, login, sid string) error
}

// Submission wires RFC 8621 §7 to the protocol.submission relay and limits.
type Submission struct {
	Sender        Sender
	MaxSize       int64 // submission_max_mail_size; 0 for none
	MaxRecipients int   // submission_max_recipients; 0 for none
	AddReceived   bool  // submission_add_received_header
	Hostname      string
	// TTL keeps a record after sending (jmap_submission_ttl_seconds); 0 keeps none.
	TTL time.Duration
}

func (o *Submission) capability() *jmapcore.SubmissionCapability {
	ext := map[string][]string{}
	if o.MaxSize > 0 {
		ext["SIZE"] = []string{strconv.FormatInt(o.MaxSize, 10)}
	}
	return &jmapcore.SubmissionCapability{MaxDelayedSend: 0, SubmissionExtensions: ext}
}

func (s *Server) submissionRegistry(lazy *lazyStore, accountID string) jmapcore.Registry {
	bind := func(fn func(context.Context, *userHandle, *lazyStore, string, json.RawMessage) (any, *jmapcore.MethodError)) jmapcore.MethodEntry {
		return jmapcore.MethodEntry{Capability: jmapcore.CapSubmission, Fn: func(ctx context.Context, args json.RawMessage) (any, *jmapcore.MethodError) {
			return s.withStore(lazy, accountID, func(h *userHandle) (any, *jmapcore.MethodError) {
				return fn(ctx, h, lazy, accountID, args)
			})
		}}
	}
	plain := func(fn func(context.Context, *userHandle, string, json.RawMessage) (any, *jmapcore.MethodError)) jmapcore.MethodEntry {
		return bind(func(ctx context.Context, h *userHandle, _ *lazyStore, a string, args json.RawMessage) (any, *jmapcore.MethodError) {
			return fn(ctx, h, a, args)
		})
	}
	return jmapcore.Registry{
		"Identity/get":                 plain(s.identityGet),
		"Identity/changes":             plain(s.identityChanges),
		"Identity/set":                 plain(s.identitySet),
		"EmailSubmission/get":          plain(s.submissionGet),
		"EmailSubmission/changes":      plain(s.submissionChanges),
		"EmailSubmission/query":        plain(s.submissionQuery),
		"EmailSubmission/queryChanges": plain(s.submissionQueryChanges),
		"EmailSubmission/set":          bind(s.submissionSet),
	}
}

// ---- records (INTERNALS §41) ----

const (
	submissionLockTTL   = 15 * time.Second
	submissionLockRenew = 5 * time.Second
	submissionLockWait  = 10 * time.Second
	submissionFile      = "jmap-submissions.json"
)

type submissionRecords struct {
	State   uint64                     `json:"state"`
	Next    uint64                     `json:"next"`
	Records []jmapcore.EmailSubmission `json:"records"`
}

// withRecords runs fn on the records under their lock, after expiry, and
// writes them back when either changed them (INTERNALS §41).
func (s *Server) withRecords(ctx context.Context, h *userHandle, fn func(*submissionRecords) (bool, error)) error {
	root := mailbox.ControlRoot(h.info)
	if root == "" {
		return fmt.Errorf("jmap: no control root for %s", h.info.Username)
	}
	path := filepath.Join(root, submissionFile)
	wrote := false
	defer func() {
		if wrote {
			locks.EmitBest(s.opts.Storage.Locker, locks.SubmissionsKey(h.info.Username), locks.EventSubmission, "")
		}
	}()
	run := func(context.Context) error {
		var recs submissionRecords
		raw, err := os.ReadFile(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return fmt.Errorf("jmap: read submissions: %w", err)
		default:
			if err := json.Unmarshal(raw, &recs); err != nil {
				return fmt.Errorf("jmap: decode submissions: %w", err)
			}
		}
		expired := s.expire(&recs, time.Now())
		changed, err := fn(&recs)
		if err != nil {
			return err
		}
		if !changed && !expired {
			return nil
		}
		b, _ := json.Marshal(recs)
		if err := os.MkdirAll(root, 0o700); err != nil {
			return fmt.Errorf("jmap: submissions dir: %w", err)
		}
		if err := os.WriteFile(path+".tmp", b, 0o600); err != nil {
			return fmt.Errorf("jmap: write submissions: %w", err)
		}
		if err := os.Rename(path+".tmp", path); err != nil {
			return err
		}
		wrote = true
		return nil
	}
	locker := s.opts.Storage.Locker
	if locker == nil {
		return run(ctx)
	}
	return locks.WithLockWaiting(locks.WithSite(ctx, "jmap-submission"), locker, locks.SubmissionsKey(h.info.Username),
		locks.Owner(h.info.Username, h.info.LockID()), submissionLockTTL, submissionLockRenew, submissionLockWait, run)
}

func (s *Server) expire(recs *submissionRecords, now time.Time) bool {
	ttl := s.opts.Submission.TTL
	kept := recs.Records[:0]
	for _, r := range recs.Records {
		if ttl > 0 && now.Sub(r.SendAt) < ttl {
			kept = append(kept, r)
		}
	}
	gone := len(kept) != len(recs.Records)
	recs.Records = kept
	if gone {
		recs.State++
	}
	return gone
}

func (s *Server) readRecords(ctx context.Context, h *userHandle) (submissionRecords, error) {
	var out submissionRecords
	err := s.withRecords(ctx, h, func(r *submissionRecords) (bool, error) {
		out = *r
		out.Records = slices.Clone(r.Records)
		return false, nil
	})
	return out, err
}

// ---- get / changes / query ----

func (s *Server) submissionGet(ctx context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req jmapcore.GetRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	resolveGet(ctx, &req)
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	props := propsOfGet(req)
	if unknown := jmapcore.UnknownProperties(jmapcore.EmailSubmission{}, props); len(unknown) > 0 {
		return nil, jmapcore.InvalidProperties(unknown)
	}
	recs, err := s.readRecords(ctx, h)
	if err != nil {
		return nil, storeFailure("EmailSubmission/get failed", accountID, err)
	}
	resp := jmapcore.GetResponse[any]{AccountID: accountID, State: strconv.FormatUint(recs.State, 10), List: []any{}, NotFound: []string{}}
	for _, r := range recs.Records {
		if req.IDs == nil || slices.Contains(*req.IDs, r.ID) {
			resp.List = append(resp.List, jmapcore.Project(r, props, nil))
		}
	}
	if req.IDs != nil {
		for _, want := range *req.IDs {
			if !slices.ContainsFunc(recs.Records, func(r jmapcore.EmailSubmission) bool { return r.ID == want }) {
				resp.NotFound = append(resp.NotFound, want)
			}
		}
	}
	return resp, nil
}

// submissionChanges keeps no history: only the current state is answered.
func (s *Server) submissionChanges(ctx context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req jmapcore.ChangesRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	recs, err := s.readRecords(ctx, h)
	if err != nil {
		return nil, storeFailure("EmailSubmission/changes failed", accountID, err)
	}
	state := strconv.FormatUint(recs.State, 10)
	if req.SinceState != state {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrCannotCalculateChanges}
	}
	return jmapcore.ChangesResponse{AccountID: accountID, OldState: state, NewState: state,
		Created: []string{}, Updated: []string{}, Destroyed: []string{}}, nil
}

type submissionFilter struct {
	IdentityIDs []string   `json:"identityIds"`
	EmailIDs    []string   `json:"emailIds"`
	ThreadIDs   []string   `json:"threadIds"`
	UndoStatus  *string    `json:"undoStatus"`
	Before      *time.Time `json:"before"`
	After       *time.Time `json:"after"`
}

func (f *submissionFilter) match(r jmapcore.EmailSubmission) bool {
	switch {
	case f.IdentityIDs != nil && !slices.Contains(f.IdentityIDs, r.IdentityID),
		f.EmailIDs != nil && !slices.Contains(f.EmailIDs, r.EmailID),
		f.ThreadIDs != nil && !slices.Contains(f.ThreadIDs, r.ThreadID),
		f.UndoStatus != nil && *f.UndoStatus != r.UndoStatus,
		f.Before != nil && !r.SendAt.Before(*f.Before),
		f.After != nil && r.SendAt.Before(*f.After):
		return false
	}
	return true
}

func (s *Server) submissionQuery(ctx context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req struct {
		jmapcore.QueryRequest
		Filter map[string]json.RawMessage `json:"filter"`
		Sort   []jmapcore.Comparator      `json:"sort"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	var f submissionFilter
	for k := range req.Filter {
		switch k {
		case "identityIds", "emailIds", "threadIds", "undoStatus", "before", "after":
		default:
			return nil, &jmapcore.MethodError{Type: jmapcore.ErrUnsupportedFilter, Description: fmt.Sprintf("cannot filter on %q", k)}
		}
	}
	if req.Filter != nil {
		raw, _ := json.Marshal(req.Filter)
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
		}
	}
	jmapcore.ResolveIDs(ctx, f.IdentityIDs)
	jmapcore.ResolveIDs(ctx, f.EmailIDs)
	jmapcore.ResolveIDs(ctx, f.ThreadIDs)
	for _, c := range req.Sort {
		switch c.Property {
		case "emailId", "threadId", "sentAt", "sendAt":
		default:
			return nil, &jmapcore.MethodError{Type: jmapcore.ErrUnsupportedSort, Description: fmt.Sprintf("cannot sort on %q", c.Property)}
		}
	}
	recs, err := s.readRecords(ctx, h)
	if err != nil {
		return nil, storeFailure("EmailSubmission/query failed", accountID, err)
	}
	matched := []jmapcore.EmailSubmission{}
	for _, r := range recs.Records {
		if f.match(r) {
			matched = append(matched, r)
		}
	}
	key := func(r jmapcore.EmailSubmission, prop string) string {
		switch prop {
		case "emailId":
			return r.EmailID
		case "threadId":
			return r.ThreadID
		}
		return r.SendAt.UTC().Format(time.RFC3339Nano)
	}
	sort.SliceStable(matched, func(i, j int) bool {
		for _, c := range req.Sort {
			a, b := key(matched[i], c.Property), key(matched[j], c.Property)
			if a == b {
				continue
			}
			if c.Ascending() {
				return a < b
			}
			return a > b
		}
		return false
	})
	ids := make([]string, 0, len(matched))
	for _, r := range matched {
		ids = append(ids, r.ID)
	}
	total := uint(len(ids))
	pos := req.Position
	if pos < 0 {
		pos = max(len(ids)+pos, 0)
	}
	pos = min(pos, len(ids))
	ids = ids[pos:]
	if req.Limit != nil && uint(len(ids)) > *req.Limit {
		ids = ids[:*req.Limit]
	}
	resp := jmapcore.QueryResponse{AccountID: accountID, QueryState: strconv.FormatUint(recs.State, 10), Position: pos, IDs: ids, Limit: req.Limit}
	if req.CalculateTotal {
		resp.Total = &total
	}
	return resp, nil
}

func (s *Server) submissionQueryChanges(_ context.Context, _ *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	return nil, &jmapcore.MethodError{Type: jmapcore.ErrCannotCalculateChanges}
}

// ---- set ----

type submissionSetRequest struct {
	jmapcore.SetRequest
	OnSuccessUpdateEmail  map[string]json.RawMessage `json:"onSuccessUpdateEmail"`
	OnSuccessDestroyEmail []string                   `json:"onSuccessDestroyEmail"`
}

// relayFailure is a temporary refusal before anything went, answered at the
// method level so the client retries rather than taking it for forbidden.
type relayFailure struct{ err error }

func (e *relayFailure) Error() string { return e.err.Error() }

// submissionSet implements EmailSubmission/set (RFC 8621 §7.5).
func (s *Server) submissionSet(ctx context.Context, h *userHandle, lazy *lazyStore, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req submissionSetRequest
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
	var resp *jmapcore.SetResponse
	var temp *relayFailure
	created := map[string]jmapcore.EmailSubmission{} // by creation id
	err := s.withRecords(ctx, h, func(recs *submissionRecords) (bool, error) {
		state := strconv.FormatUint(recs.State, 10)
		if req.IfInState != nil && *req.IfInState != state {
			return false, errStateMismatch
		}
		resp = jmapcore.NewSetResponse(accountID, state)
		changed := false
		cids := sortedKeys(req.Create)
		for i, cid := range cids {
			raw := resolveProperty(ctx, resolveProperty(ctx, req.Create[cid], "identityId"), "emailId")
			rec, serr, err := s.submissionCreate(ctx, h, lazy, raw)
			if err != nil && len(created) == 0 {
				temp = &relayFailure{err}
				break
			}
			if err != nil {
				// Some already went: report them, and leave this and the rest unsent.
				slog.Warn("jmap: EmailSubmission relay temporarily refused", "session", lazy.sessionID, "sent_before_it", len(created))
				merr := storeFailure("EmailSubmission/set relay", accountID, err)
				for _, rest := range cids[i:] {
					resp.NotCreated[rest] = &jmapcore.SetError{Type: merr.Type, Description: "the relay refused for now; not sent, try again"}
				}
				break
			}
			if serr != nil {
				resp.NotCreated[cid] = serr
				continue
			}
			recs.Next++
			rec.ID = "ES" + strconv.FormatUint(recs.Next, 10)
			created[cid] = rec
			jmapcore.AddCreated(ctx, cid, rec.ID)
			if s.opts.Submission.TTL > 0 {
				recs.Records = append(recs.Records, rec)
				changed = true
			}
			resp.Created[cid] = map[string]any{"id": rec.ID, "threadId": rec.ThreadID, "sendAt": rec.SendAt,
				"undoStatus": rec.UndoStatus, "envelope": rec.Envelope}
		}
		if temp == nil {
			for _, id := range sortedKeys(req.Update) {
				i := slices.IndexFunc(recs.Records, func(r jmapcore.EmailSubmission) bool { return r.ID == id })
				if i < 0 {
					resp.NotUpdated[id] = &jmapcore.SetError{Type: jmapcore.SetErrNotFound}
					continue
				}
				resp.NotUpdated[id] = submissionUpdate(req.Update[id])
			}
			for _, id := range req.Destroy {
				i := slices.IndexFunc(recs.Records, func(r jmapcore.EmailSubmission) bool { return r.ID == id })
				if i < 0 {
					resp.NotDestroyed[id] = &jmapcore.SetError{Type: jmapcore.SetErrNotFound}
					continue
				}
				recs.Records = slices.Delete(recs.Records, i, i+1)
				resp.Destroyed = append(resp.Destroyed, id)
				changed = true
			}
		}
		if changed {
			recs.State++
		}
		resp.NewState = strconv.FormatUint(recs.State, 10)
		return changed, nil
	})
	switch {
	case errors.Is(err, errStateMismatch):
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrStateMismatch}
	case err != nil:
		return nil, storeFailure("EmailSubmission/set", accountID, err)
	case temp != nil:
		slog.Warn("jmap: EmailSubmission relay temporarily refused", "session", lazy.sessionID, "sent_before_it", 0)
		return nil, storeFailure("EmailSubmission/set relay", accountID, temp.err)
	}
	return s.submissionOnSuccess(ctx, h, accountID, &req, created, resp)
}

var errStateMismatch = errors.New("jmap: state mismatch")

// submissionUpdate: every property is immutable or server-set; undoStatus
// cannot go to canceled, since nothing waits to be sent (maxDelayedSend 0).
func submissionUpdate(raw json.RawMessage) *jmapcore.SetError {
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(raw, &patch); err != nil {
		return &jmapcore.SetError{Type: jmapcore.SetErrInvalidPatch, Description: err.Error()}
	}
	if v, ok := patch["undoStatus"]; ok && string(v) == `"canceled"` && len(patch) == 1 {
		return &jmapcore.SetError{Type: jmapcore.SetErrCannotUnsend}
	}
	return &jmapcore.SetError{Type: jmapcore.SetErrInvalidProps, Properties: sortedKeys(patch)}
}

// submissionOnSuccess runs the one implicit Email/set the two onSuccess
// arguments ask for, answered right after EmailSubmission/set (RFC 8621 §7.5).
func (s *Server) submissionOnSuccess(ctx context.Context, h *userHandle, accountID string, req *submissionSetRequest,
	created map[string]jmapcore.EmailSubmission, resp *jmapcore.SetResponse) (any, *jmapcore.MethodError) {
	if len(req.OnSuccessUpdateEmail) == 0 && len(req.OnSuccessDestroyEmail) == 0 {
		return resp, nil
	}
	emailOf := func(ref string) (string, bool) {
		if cid, ok := strings.CutPrefix(ref, "#"); ok {
			rec, ok := created[cid]
			return rec.EmailID, ok
		}
		for _, rec := range created {
			if rec.ID == ref {
				return rec.EmailID, true
			}
		}
		return "", false
	}
	update := map[string]json.RawMessage{}
	for _, ref := range sortedKeys(req.OnSuccessUpdateEmail) {
		if id, ok := emailOf(ref); ok {
			update[id] = req.OnSuccessUpdateEmail[ref]
		}
	}
	destroy := []string{}
	for _, ref := range req.OnSuccessDestroyEmail {
		if id, ok := emailOf(ref); ok {
			destroy = append(destroy, id)
		}
	}
	if len(update) == 0 && len(destroy) == 0 {
		return resp, nil
	}
	args, _ := json.Marshal(map[string]any{"accountId": accountID, "update": update, "destroy": destroy})
	result, merr := s.emailSet(ctx, h, accountID, args)
	then := jmapcore.Implicit{Name: "Email/set", Result: result}
	if merr != nil {
		then = jmapcore.Implicit{Name: "error", Result: merr}
	}
	return &jmapcore.FollowedBy{Result: resp, Then: []jmapcore.Implicit{then}}, nil
}

type submissionProps struct {
	IdentityID *string            `json:"identityId"`
	EmailID    *string            `json:"emailId"`
	Envelope   *jmapcore.Envelope `json:"envelope"`
}

// submissionCreate checks and sends one message. A SetError refuses this
// object; an error is the relay's temporary refusal, which stops sending.
func (s *Server) submissionCreate(_ context.Context, h *userHandle, lazy *lazyStore, raw json.RawMessage) (jmapcore.EmailSubmission, *jmapcore.SetError, error) {
	var none jmapcore.EmailSubmission
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return none, &jmapcore.SetError{Type: jmapcore.SetErrInvalidProps, Description: err.Error()}, nil
	}
	var bad []string
	for k := range fields {
		if k != "identityId" && k != "emailId" && k != "envelope" {
			bad = append(bad, k)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return none, &jmapcore.SetError{Type: jmapcore.SetErrInvalidProps, Properties: bad}, nil
	}
	var p submissionProps
	if err := json.Unmarshal(raw, &p); err != nil {
		return none, &jmapcore.SetError{Type: jmapcore.SetErrInvalidProps, Description: err.Error()}, nil
	}
	if p.IdentityID == nil {
		return none, invalidProp("identityId", "identityId is required"), nil
	}
	i := slices.IndexFunc(h.identities(), func(id jmapcore.Identity) bool { return id.ID == *p.IdentityID })
	if i < 0 {
		return none, invalidProp("identityId", "no such identity"), nil
	}
	identity := h.identities()[i]
	if p.EmailID == nil {
		return none, invalidProp("emailId", "emailId is required"), nil
	}
	found, err := s.findMessages(h, map[string]bool{*p.EmailID: true})
	ref, ok := found[*p.EmailID]
	if err != nil || !ok {
		return none, invalidProp("emailId", "no such email"), nil
	}
	rc, err := h.mbox.OpenMessage(ref.folder, ref.meta)
	if err != nil {
		return none, invalidProp("emailId", "the email cannot be read"), nil
	}
	limit := s.opts.Submission.MaxSize
	readMax := int64(1 << 40)
	if limit > 0 {
		readMax = limit + 1
	}
	body, err := io.ReadAll(io.LimitReader(rc, readMax))
	rc.Close() //nolint:errcheck
	if err != nil {
		return none, invalidProp("emailId", "the email cannot be read"), nil
	}
	if limit > 0 && int64(len(body)) > limit {
		return none, &jmapcore.SetError{Type: jmapcore.SetErrTooLarge, MaxSize: &limit}, nil
	}
	msg, err := mail.ReadMessage(bytes.NewReader(body))
	if err != nil {
		return none, &jmapcore.SetError{Type: jmapcore.SetErrInvalidEmail, Properties: []string{"headers"}, Description: err.Error()}, nil
	}
	if serr := checkFrom(msg.Header, identity); serr != nil {
		return none, serr, nil
	}
	env, serr := envelopeFor(p.Envelope, msg.Header, identity)
	if serr != nil {
		return none, serr, nil
	}
	if serr := s.checkRecipients(env); serr != nil {
		return none, serr, nil
	}
	out := withoutBcc(body)
	if s.opts.Submission.AddReceived {
		out = append([]byte(s.receivedHeader(lazy.clientIP)), out...)
	}
	rcpts := make([]string, 0, len(env.RcptTo))
	for _, r := range env.RcptTo {
		rcpts = append(rcpts, r.Email)
	}
	if err := s.opts.Submission.Sender.Send(env.MailFrom.Email, rcpts, bytes.NewReader(out), net.ParseIP(lazy.clientIP), lazy.user, lazy.sessionID); err != nil {
		var se *goSmtp.SMTPError
		if errors.As(err, &se) && se.Code >= 500 {
			slog.Info("jmap: EmailSubmission refused by the relay", "user", lazy.user, "session", lazy.sessionID, "err", err)
			return none, &jmapcore.SetError{Type: jmapcore.SetErrForbiddenToSend,
				Description: fmt.Sprintf("%d %d.%d.%d %s", se.Code, se.EnhancedCode[0], se.EnhancedCode[1], se.EnhancedCode[2], se.Message)}, nil
		}
		return none, nil, err
	}
	slog.Info("jmap: EmailSubmission sent", "user", lazy.user, "session", lazy.sessionID, "email", *p.EmailID, "rcpts", len(rcpts))
	return jmapcore.EmailSubmission{IdentityID: identity.ID, EmailID: *p.EmailID, ThreadID: h.threadOf(*p.EmailID),
		Envelope: env, SendAt: time.Now().UTC().Truncate(time.Second), UndoStatus: "final",
		DsnBlobIDs: []string{}, MdnBlobIDs: []string{}}, nil, nil
}

// checkFrom refuses a From the identity does not allow (forbiddenFrom).
func checkFrom(hdr mail.Header, identity jmapcore.Identity) *jmapcore.SetError {
	list, err := hdr.AddressList("From")
	if err != nil || len(list) == 0 {
		return &jmapcore.SetError{Type: jmapcore.SetErrInvalidEmail, Properties: []string{"from"}}
	}
	for _, a := range list {
		if !allowedBy(identity, a.Address) {
			return &jmapcore.SetError{Type: jmapcore.SetErrForbiddenFrom, Description: a.Address + " is not this identity's address"}
		}
	}
	return nil
}

// envelopeFor checks a client's envelope, or builds one from the message as
// RFC 8621 §7 says; a disallowed sender address becomes the identity's.
func envelopeFor(given *jmapcore.Envelope, hdr mail.Header, identity jmapcore.Identity) (*jmapcore.Envelope, *jmapcore.SetError) {
	if given != nil {
		if !allowedBy(identity, given.MailFrom.Email) {
			return nil, &jmapcore.SetError{Type: jmapcore.SetErrForbiddenMailFrom}
		}
		for _, a := range append([]jmapcore.SubmissionAddress{given.MailFrom}, given.RcptTo...) {
			for k := range a.Parameters {
				if !strings.EqualFold(k, "SIZE") {
					return nil, invalidProp("envelope", "unsupported parameter "+k)
				}
			}
		}
		return given, nil
	}
	field := "Sender"
	if len(hdr["Sender"]) == 0 {
		field = "From"
	}
	if len(hdr[field]) > 1 {
		return nil, &jmapcore.SetError{Type: jmapcore.SetErrInvalidEmail, Properties: []string{strings.ToLower(field)}}
	}
	list, err := hdr.AddressList(field)
	if err != nil || len(list) != 1 {
		return nil, &jmapcore.SetError{Type: jmapcore.SetErrInvalidEmail, Properties: []string{strings.ToLower(field)}}
	}
	from := list[0].Address
	if !allowedBy(identity, from) {
		from = identity.Email
	}
	env := &jmapcore.Envelope{MailFrom: jmapcore.SubmissionAddress{Email: from}, RcptTo: []jmapcore.SubmissionAddress{}}
	seen := map[string]bool{}
	for _, f := range []string{"To", "Cc", "Bcc"} {
		addrs, _ := hdr.AddressList(f)
		for _, a := range addrs {
			if key := strings.ToLower(a.Address); !seen[key] {
				seen[key] = true
				env.RcptTo = append(env.RcptTo, jmapcore.SubmissionAddress{Email: a.Address})
			}
		}
	}
	return env, nil
}

func (s *Server) checkRecipients(env *jmapcore.Envelope) *jmapcore.SetError {
	if len(env.RcptTo) == 0 {
		return &jmapcore.SetError{Type: jmapcore.SetErrNoRecipients}
	}
	var invalid []string
	for _, r := range env.RcptTo {
		if a, err := mail.ParseAddress(r.Email); err != nil || a.Address != r.Email {
			invalid = append(invalid, r.Email)
		}
	}
	if len(invalid) > 0 {
		return &jmapcore.SetError{Type: jmapcore.SetErrInvalidRecipients, InvalidRecipients: invalid}
	}
	if max := s.opts.Submission.MaxRecipients; max > 0 && len(env.RcptTo) > max {
		return &jmapcore.SetError{Type: jmapcore.SetErrTooManyRecipients, MaxRecipients: &max}
	}
	return nil
}

// withoutBcc drops every Bcc header field, continuation lines included, and
// leaves the rest of the message byte for byte (RFC 8621 §7.5).
func withoutBcc(msg []byte) []byte {
	end := bytes.Index(msg, []byte("\r\n\r\n"))
	sep := 4
	if end < 0 {
		end, sep = bytes.Index(msg, []byte("\n\n")), 2
	}
	if end < 0 {
		return msg
	}
	var out []byte
	dropping := false
	for _, line := range bytes.SplitAfter(msg[:end+sep/2], []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if !dropping {
				out = append(out, line...)
			}
			continue
		}
		dropping = len(line) >= 4 && strings.EqualFold(string(line[:4]), "bcc:")
		if !dropping {
			out = append(out, line...)
		}
	}
	return append(out, msg[end+sep/2:]...)
}

func (s *Server) receivedHeader(clientIP string) string {
	host := s.opts.Submission.Hostname
	if host == "" {
		host = "yarilo"
	}
	return fmt.Sprintf("Received: from [%s]\r\n\tby %s with HTTP;\r\n\t%s\r\n", clientIP, host, time.Now().UTC().Format(time.RFC1123Z))
}
