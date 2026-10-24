package jmap

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

const identitiesFile = "jmap-identities.json"

// storedIdentities is the per-user file: the identities a client created and
// the changes made to the ones the account derives (RFC 8621 §6.3).
type storedIdentities struct {
	Identities []jmapcore.Identity `json:"identities"`
}

// identityID names an address for good: the hash of its lower-cased form.
func identityID(addr string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(addr)))
	return "I" + hex.EncodeToString(sum[:8])
}

// newIdentityID is a created identity's id: longer than a derived one, so the
// two never meet.
func newIdentityID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "I" + hex.EncodeToString(b[:])
}

// derivedIdentities are the login when it is an address, then the userdb
// identity_aliases, each once whatever its case (RFC 8621 §6).
func derivedIdentities(user string, aliases []string) []jmapcore.Identity {
	var out []jmapcore.Identity
	seen := map[string]bool{}
	add := func(addr string) {
		if key := strings.ToLower(addr); !seen[key] {
			seen[key] = true
			out = append(out, jmapcore.Identity{ID: identityID(addr), Email: addr, ReplyTo: nil, Bcc: nil})
		}
	}
	if strings.Contains(user, "@") {
		add(user)
	}
	for _, a := range aliases {
		parsed, err := mail.ParseAddress(a)
		if err != nil || parsed.Name != "" || parsed.Address != a {
			slog.Warn("jmap: identity alias dropped: not an address", "user", user, "alias", a, "err", err)
			continue
		}
		add(a)
	}
	return out
}

// mergeIdentities lays the stored changes over the derived identities, then
// adds the created ones whose address the account still has.
func mergeIdentities(derived []jmapcore.Identity, stored []jmapcore.Identity) []jmapcore.Identity {
	byID := map[string]jmapcore.Identity{}
	for _, s := range stored {
		byID[s.ID] = s
	}
	out := make([]jmapcore.Identity, 0, len(derived)+len(stored))
	for _, d := range derived {
		if s, ok := byID[d.ID]; ok && !s.MayDelete {
			s.Email, s.MayDelete = d.Email, false
			d = s
		}
		out = append(out, d)
	}
	for _, s := range stored {
		if s.MayDelete && addressOf(derived, s.Email) != "" {
			out = append(out, s)
		}
	}
	return out
}

// addressOf returns the derived address addr names, in its derived case.
func addressOf(derived []jmapcore.Identity, addr string) string {
	for _, d := range derived {
		if strings.EqualFold(d.Email, addr) {
			return d.Email
		}
	}
	return ""
}

// identityState moves with any property of any identity.
func identityState(ids []jmapcore.Identity) string {
	b, _ := json.Marshal(ids)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func (h *userHandle) derivedIdentities() []jmapcore.Identity {
	return derivedIdentities(h.user, h.info.IdentityAliases)
}

func (h *userHandle) identitiesPath() (string, error) {
	root := mailbox.ControlRoot(h.info)
	if root == "" {
		return "", fmt.Errorf("jmap: no control root for %s", h.info.Username)
	}
	return filepath.Join(root, identitiesFile), nil
}

// readStoredIdentities reads without the lock: a write replaces the file by
// rename, so a reader sees one version or the other.
func readStoredIdentities(path string) (storedIdentities, error) {
	var st storedIdentities
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return st, nil
	case err != nil:
		return st, fmt.Errorf("jmap: read identities: %w", err)
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("jmap: decode identities: %w", err)
	}
	return st, nil
}

// identities is every identity of the account.
func (h *userHandle) identities() ([]jmapcore.Identity, error) {
	path, err := h.identitiesPath()
	if err != nil {
		return nil, err
	}
	st, err := readStoredIdentities(path)
	if err != nil {
		return nil, err
	}
	return mergeIdentities(h.derivedIdentities(), st.Identities), nil
}

// withIdentities runs fn on the stored identities under their lock and writes
// them back when fn changed them; the write is announced once.
func (s *Server) withIdentities(ctx context.Context, h *userHandle, fn func(*storedIdentities) (bool, error)) error {
	path, err := h.identitiesPath()
	if err != nil {
		return err
	}
	wrote := false
	defer func() {
		if wrote {
			locks.EmitBest(s.opts.Storage.Locker, locks.IdentitiesKey(h.info.Username), locks.EventIdentities, "")
		}
	}()
	run := func(context.Context) error {
		st, err := readStoredIdentities(path)
		if err != nil {
			return err
		}
		changed, err := fn(&st)
		if err != nil || !changed {
			return err
		}
		b, _ := json.Marshal(st)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("jmap: identities dir: %w", err)
		}
		if err := os.WriteFile(path+".tmp", b, 0o600); err != nil {
			return fmt.Errorf("jmap: write identities: %w", err)
		}
		if err := os.Rename(path+".tmp", path); err != nil {
			return fmt.Errorf("jmap: replace identities: %w", err)
		}
		wrote = true
		return nil
	}
	locker := s.opts.Storage.Locker
	if locker == nil {
		return run(ctx)
	}
	return locks.WithLockWaiting(locks.WithSite(ctx, "jmap-identity"), locker, locks.IdentitiesKey(h.info.Username),
		locks.Owner(h.info.Username, h.info.LockID()), submissionLockTTL, submissionLockRenew, submissionLockWait, run)
}

// allowedBy says whether addr is one of the identity's addresses: the identity
// is one address here, matched without regard to case.
func allowedBy(id jmapcore.Identity, addr string) bool {
	return strings.EqualFold(id.Email, addr)
}

func (s *Server) identityGet(ctx context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req jmapcore.GetRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	resolveGet(ctx, &req)
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	props := propsOfGet(req)
	if unknown := jmapcore.UnknownProperties(jmapcore.Identity{}, props); len(unknown) > 0 {
		return nil, jmapcore.InvalidProperties(unknown)
	}
	all, err := h.identities()
	if err != nil {
		return nil, storeFailure("Identity/get", accountID, err)
	}
	resp := jmapcore.GetResponse[any]{AccountID: accountID, State: identityState(all), List: []any{}, NotFound: []string{}}
	for _, id := range all {
		if req.IDs == nil || slices.Contains(*req.IDs, id.ID) {
			resp.List = append(resp.List, jmapcore.Project(id, props, nil))
		}
	}
	if req.IDs != nil {
		for _, want := range *req.IDs {
			if !slices.ContainsFunc(all, func(id jmapcore.Identity) bool { return id.ID == want }) {
				resp.NotFound = append(resp.NotFound, want)
			}
		}
	}
	return resp, nil
}

// identityChanges answers from the state alone: no history is kept, so any
// state but the current one cannot be calculated from.
func (s *Server) identityChanges(_ context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req jmapcore.ChangesRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	all, err := h.identities()
	if err != nil {
		return nil, storeFailure("Identity/changes", accountID, err)
	}
	state := identityState(all)
	if req.SinceState != state {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrCannotCalculateChanges}
	}
	return jmapcore.ChangesResponse{AccountID: accountID, OldState: state, NewState: state,
		Created: []string{}, Updated: []string{}, Destroyed: []string{}}, nil
}

// identityWritable are the properties a client sets; email only on create.
var identityWritable = map[string]bool{
	"name": true, "email": true, "replyTo": true, "bcc": true, "textSignature": true, "htmlSignature": true,
}

// identityPatch reads the properties of a create or an update into id, naming
// every property it refuses.
func identityPatch(raw json.RawMessage, id *jmapcore.Identity, create bool) *jmapcore.SetError {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return invalidProp("", err.Error())
	}
	var bad []string
	for k, v := range fields {
		if !identityWritable[k] || (k == "email" && !create) {
			bad = append(bad, k)
			continue
		}
		var err error
		switch k {
		case "name":
			err = json.Unmarshal(v, &id.Name)
		case "email":
			err = json.Unmarshal(v, &id.Email)
		case "replyTo":
			id.ReplyTo = nil
			err = json.Unmarshal(v, &id.ReplyTo)
		case "bcc":
			id.Bcc = nil
			err = json.Unmarshal(v, &id.Bcc)
		case "textSignature":
			err = json.Unmarshal(v, &id.TextSignature)
		case "htmlSignature":
			err = json.Unmarshal(v, &id.HTMLSignature)
		}
		if err != nil || (string(v) == "null" && (k == "name" || k == "email" || strings.HasSuffix(k, "Signature"))) {
			bad = append(bad, k)
		}
	}
	if create {
		if _, ok := fields["email"]; !ok {
			bad = append(bad, "email")
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return &jmapcore.SetError{Type: jmapcore.SetErrInvalidProps, Properties: bad}
	}
	return nil
}

// identitySet implements Identity/set (RFC 8621 §6.3): created identities may
// be destroyed, the derived ones only changed.
func (s *Server) identitySet(ctx context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req jmapcore.SetRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	resolveSet(ctx, &req)
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	if max := s.opts.Limits.MaxObjectsInSet; max > 0 && len(req.Create)+len(req.Update)+len(req.Destroy) > max {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrRequestTooLarge}
	}
	derived := h.derivedIdentities()
	var resp *jmapcore.SetResponse
	var merr *jmapcore.MethodError
	err := s.withIdentities(ctx, h, func(st *storedIdentities) (bool, error) {
		state := identityState(mergeIdentities(derived, st.Identities))
		if req.IfInState != nil && *req.IfInState != state {
			merr = &jmapcore.MethodError{Type: jmapcore.ErrStateMismatch}
			return false, nil
		}
		resp = jmapcore.NewSetResponse(accountID, state)
		changed := false
		for _, cid := range sortedKeys(req.Create) {
			id := jmapcore.Identity{MayDelete: true}
			if serr := identityPatch(req.Create[cid], &id, true); serr != nil {
				resp.NotCreated[cid] = serr
				continue
			}
			addr := addressOf(derived, id.Email)
			if addr == "" {
				resp.NotCreated[cid] = &jmapcore.SetError{Type: jmapcore.SetErrForbiddenFrom,
					Description: "the address is not one of the account's"}
				continue
			}
			id.ID, id.Email = newIdentityID(), addr
			st.Identities = append(st.Identities, id)
			changed = true
			created := map[string]any{"id": id.ID, "mayDelete": true}
			resp.Created[cid] = created
			noteCreated(ctx, cid, created)
		}
		for _, id := range sortedKeys(req.Update) {
			current := mergeIdentities(derived, st.Identities)
			i := slices.IndexFunc(current, func(c jmapcore.Identity) bool { return c.ID == id })
			if i < 0 {
				resp.NotUpdated[id] = &jmapcore.SetError{Type: jmapcore.SetErrNotFound}
				continue
			}
			next := current[i]
			if serr := identityPatch(req.Update[id], &next, false); serr != nil {
				resp.NotUpdated[id] = serr
				continue
			}
			if j := slices.IndexFunc(st.Identities, func(c jmapcore.Identity) bool { return c.ID == id }); j >= 0 {
				st.Identities[j] = next
			} else {
				st.Identities = append(st.Identities, next)
			}
			changed = true
			resp.Updated[id] = nil
		}
		for _, id := range req.Destroy {
			current := mergeIdentities(derived, st.Identities)
			i := slices.IndexFunc(current, func(c jmapcore.Identity) bool { return c.ID == id })
			switch {
			case i < 0:
				resp.NotDestroyed[id] = &jmapcore.SetError{Type: jmapcore.SetErrNotFound}
				continue
			case !current[i].MayDelete:
				resp.NotDestroyed[id] = &jmapcore.SetError{Type: jmapcore.SetErrForbidden,
					Description: "this identity is the account's own address"}
				continue
			}
			st.Identities = slices.DeleteFunc(st.Identities, func(c jmapcore.Identity) bool { return c.ID == id })
			changed = true
			resp.Destroyed = append(resp.Destroyed, id)
		}
		resp.NewState = identityState(mergeIdentities(derived, st.Identities))
		return changed, nil
	})
	if err != nil {
		return nil, storeFailure("Identity/set", accountID, err)
	}
	if merr != nil {
		return nil, merr
	}
	return resp, nil
}
