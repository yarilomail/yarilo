package jmap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/mail"
	"slices"
	"strings"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
)

// identityID names an address for good: the hash of its lower-cased form.
func identityID(addr string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(addr)))
	return "I" + hex.EncodeToString(sum[:8])
}

// identities are the login when it is an address, then the userdb
// identity_aliases, each once whatever its case (RFC 8621 §6).
func identities(user string, aliases []string) []jmapcore.Identity {
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

// identityState moves with the list of addresses and nothing else.
func identityState(ids []jmapcore.Identity) string {
	h := sha256.New()
	for _, id := range ids {
		h.Write([]byte(strings.ToLower(id.Email) + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

func (h *userHandle) identities(user string) []jmapcore.Identity {
	return identities(user, h.info.IdentityAliases)
}

// allowedBy says whether addr is one of the identity's addresses: the identity
// is one address here, matched without regard to case.
func allowedBy(id jmapcore.Identity, addr string) bool {
	return strings.EqualFold(id.Email, addr)
}

func (s *Server) identityGet(_ context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req jmapcore.GetRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	props := propsOfGet(req)
	if unknown := jmapcore.UnknownProperties(jmapcore.Identity{}, props); len(unknown) > 0 {
		return nil, jmapcore.InvalidProperties(unknown)
	}
	all := h.identities(accountID)
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
	state := identityState(h.identities(accountID))
	if req.SinceState != state {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrCannotCalculateChanges}
	}
	return jmapcore.ChangesResponse{AccountID: accountID, OldState: state, NewState: state,
		Created: []string{}, Updated: []string{}, Destroyed: []string{}}, nil
}

// identitySet refuses every object: identities come from userdb here.
func (s *Server) identitySet(_ context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req jmapcore.SetRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	state := identityState(h.identities(accountID))
	if req.IfInState != nil && *req.IfInState != state {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrStateMismatch}
	}
	resp := jmapcore.NewSetResponse(accountID, state)
	refuse := &jmapcore.SetError{Type: jmapcore.SetErrForbidden, Description: "identities come from the account's userdb"}
	for cid := range req.Create {
		resp.NotCreated[cid] = refuse
	}
	for id := range req.Update {
		resp.NotUpdated[id] = refuse
	}
	for _, id := range req.Destroy {
		resp.NotDestroyed[id] = refuse
	}
	return resp, nil
}
