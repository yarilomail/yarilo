package jmap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	gosieve "github.com/foxcpp/go-sieve"

	"github.com/yarilomail/yarilo/internal/sieve"
	"github.com/yarilomail/yarilo/pkg/jmapcore"
)

// SieveScripts wires RFC 9661 to the script store ManageSieve uses, so a script
// written over one protocol is the script the other one lists.
type SieveScripts struct {
	Store *sieve.IndexedStore
	// Quota is sieve_max_script_size, sieve_max_num_scripts, sieve_max_storage.
	Quota sieve.Quota
	// Extensions is what a user's script may use (sieve.UserExtensions).
	Extensions   sieve.ExtensionSet
	MaxRedirects int
	// Implementation is the session capability's "implementation".
	Implementation string
	// Vacation serves VacationResponse on these scripts (sieve_vacation_enabled).
	Vacation bool
}

// maxSieveNameOctets is the RFC 9661 floor; a name also stays within the
// ManageSieve character limit.
const maxSieveNameOctets = 512

func optionalLimit(n int) *int {
	if n <= 0 {
		return nil
	}
	return &n
}

func (o *SieveScripts) session() *jmapcore.SieveSession {
	exts := sieve.EffectiveExtensions(o.Extensions)
	acct := jmapcore.SieveCapability{
		MaxSizeScriptName:  maxSieveNameOctets,
		MaxSizeScript:      optionalLimit(o.Quota.MaxSize),
		MaxNumberScripts:   optionalLimit(o.Quota.MaxScripts),
		MaxNumberRedirects: optionalLimit(o.MaxRedirects),
		SieveExtensions:    exts,
	}
	if slices.Contains(exts, "enotify") {
		acct.NotificationMethods = []string{"mailto"}
	}
	return &jmapcore.SieveSession{Implementation: o.Implementation, Account: acct}
}

func (s *Server) sieveRegistry(lazy *lazyStore, accountID string) jmapcore.Registry {
	bind := func(fn func(context.Context, *userHandle, string, json.RawMessage) (any, *jmapcore.MethodError)) jmapcore.MethodEntry {
		return jmapcore.MethodEntry{Capability: jmapcore.CapSieve, Fn: func(ctx context.Context, args json.RawMessage) (any, *jmapcore.MethodError) {
			return s.withStore(lazy, accountID, func(h *userHandle) (any, *jmapcore.MethodError) {
				return fn(ctx, h, accountID, args)
			})
		}}
	}
	return jmapcore.Registry{
		"SieveScript/get":      bind(s.sieveGet),
		"SieveScript/set":      bind(s.sieveSet),
		"SieveScript/query":    bind(s.sieveQuery),
		"SieveScript/validate": bind(s.sieveValidate),
	}
}

// scriptBlobID names a script's bytes by content, so a new version is a new blob.
func scriptBlobID(src []byte) string {
	sum := sha256.Sum256(src)
	return "V" + hex.EncodeToString(sum[:]) + "-" + strconv.Itoa(len(src))
}

func isScriptBlobID(id string) bool {
	return strings.HasPrefix(id, "V") && strings.Contains(id, "-")
}

// sieveView is one reading of the account's scripts.
type sieveView struct {
	state   string
	ix      sieve.ScriptIndex
	active  string
	scripts []jmapcore.SieveScript
	src     map[string][]byte // by id
}

func (s *Server) sieveRead(ctx context.Context, h *userHandle, user string) (sieveView, error) {
	st := s.opts.Sieve.Store
	ix, err := st.Index(ctx, user, h.info.Home)
	if err != nil {
		return sieveView{}, err
	}
	active, err := st.ActiveScriptName(ctx, user, h.info.Home)
	if err != nil {
		return sieveView{}, err
	}
	v := sieveView{state: strconv.FormatUint(ix.State, 10), ix: ix, active: active, src: map[string][]byte{}}
	for name, id := range ix.IDs {
		src, found, err := st.GetScript(ctx, user, h.info.Home, name)
		if err != nil {
			return sieveView{}, err
		}
		if !found {
			continue
		}
		v.src[id] = src
		v.scripts = append(v.scripts, jmapcore.SieveScript{ID: id, Name: name, BlobID: scriptBlobID(src), IsActive: name == active})
	}
	sort.Slice(v.scripts, func(i, j int) bool { return v.scripts[i].Name < v.scripts[j].Name })
	return v, nil
}

// scriptBlob finds a script's bytes by blob id among the account's scripts.
func (s *Server) scriptBlob(ctx context.Context, h *userHandle, user, blobID string) ([]byte, bool, error) {
	if s.opts.Sieve == nil || !isScriptBlobID(blobID) {
		return nil, false, nil
	}
	v, err := s.sieveRead(ctx, h, user)
	if err != nil {
		return nil, false, err
	}
	for _, sc := range v.scripts {
		if sc.BlobID == blobID {
			return v.src[sc.ID], true, nil
		}
	}
	return nil, false, nil
}

func (s *Server) sieveGet(ctx context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req jmapcore.GetRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	resolveGet(ctx, &req)
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	props := propsOfGet(req)
	if unknown := jmapcore.UnknownProperties(jmapcore.SieveScript{}, props); len(unknown) > 0 {
		return nil, jmapcore.InvalidProperties(unknown)
	}
	v, err := s.sieveRead(ctx, h, h.user)
	if err != nil {
		return nil, storeFailure("SieveScript/get failed", accountID, err)
	}
	resp := jmapcore.GetResponse[any]{AccountID: accountID, State: v.state, List: []any{}, NotFound: []string{}}
	if req.IDs == nil {
		for _, sc := range v.scripts {
			resp.List = append(resp.List, jmapcore.Project(sc, props, nil))
		}
		return resp, nil
	}
	for _, id := range *req.IDs {
		i := slices.IndexFunc(v.scripts, func(sc jmapcore.SieveScript) bool { return sc.ID == id })
		if i < 0 {
			resp.NotFound = append(resp.NotFound, id)
			continue
		}
		resp.List = append(resp.List, jmapcore.Project(v.scripts[i], props, nil))
	}
	return resp, nil
}

type sieveSetRequest struct {
	jmapcore.SetRequest
	OnSuccessActivateScript   *string `json:"onSuccessActivateScript"`
	OnSuccessDeactivateScript bool    `json:"onSuccessDeactivateScript"`
}

type sieveProps struct {
	Name   *string `json:"name"`
	BlobID *string `json:"blobId"`
}

func readSieveProps(raw json.RawMessage) (sieveProps, *jmapcore.SetError) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return sieveProps{}, &jmapcore.SetError{Type: jmapcore.SetErrInvalidProps, Description: err.Error()}
	}
	var bad []string
	for k := range fields {
		if k != "name" && k != "blobId" {
			bad = append(bad, k)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return sieveProps{}, &jmapcore.SetError{Type: jmapcore.SetErrInvalidProps, Properties: bad}
	}
	var p sieveProps
	if err := json.Unmarshal(raw, &p); err != nil {
		return sieveProps{}, &jmapcore.SetError{Type: jmapcore.SetErrInvalidProps, Description: err.Error()}
	}
	return p, nil
}

func (s *Server) checkSieveName(v sieveView, name string) *jmapcore.SetError {
	if len(name) > maxSieveNameOctets || !sieve.ValidScriptName(name) {
		return invalidProp("name", "not a valid script name")
	}
	if name == s.opts.Sieve.Store.DefaultScriptName() {
		return invalidProp("name", "the name is reserved")
	}
	if id, ok := v.ix.IDs[name]; ok {
		return &jmapcore.SetError{Type: jmapcore.SetErrAlreadyExists, ExistingID: id}
	}
	return nil
}

// taken is the alreadyExists for a name the store refused under its lock.
func (s *Server) taken(ctx context.Context, user, home, name string, v *sieveView) *jmapcore.SetError {
	fresh, err := s.opts.Sieve.Store.Index(ctx, user, home)
	return &jmapcore.SetError{Type: jmapcore.SetErrAlreadyExists, ExistingID: existingID(fresh, err, v.ix, user, name)}
}

// existingID names the script holding name: from the index read after the
// refusal, else from the one this call started with.
func existingID(fresh sieve.ScriptIndex, err error, start sieve.ScriptIndex, user, name string) string {
	if err == nil && fresh.IDs[name] != "" {
		return fresh.IDs[name]
	}
	if id := start.IDs[name]; id != "" {
		return id
	}
	slog.Warn("jmap: alreadyExists without existingId", "user", user, "script", name, "err", err)
	return ""
}

// checkSieveContent applies RFC 9661 §2.2 with the parser and extension list
// ManageSieve uses.
func (s *Server) checkSieveContent(src []byte) *jmapcore.SetError {
	if max := s.opts.Sieve.Quota.MaxSize; max > 0 && len(src) > max {
		return &jmapcore.SetError{Type: jmapcore.SetErrTooLarge}
	}
	if len(src) == 0 || !utf8.Valid(src) {
		return &jmapcore.SetError{Type: jmapcore.SetErrInvalidSieve, Description: "a script is UTF-8 of at least one character"}
	}
	compiled, err := gosieve.Load(bytes.NewReader(src), sieve.CompileOptions(s.opts.Sieve.Extensions))
	if err != nil {
		return &jmapcore.SetError{Type: jmapcore.SetErrInvalidSieve, Description: strings.TrimSpace(err.Error())}
	}
	if err := sieve.CheckExtensions(compiled, s.opts.Sieve.Extensions); err != nil {
		return &jmapcore.SetError{Type: jmapcore.SetErrInvalidSieve, Description: err.Error()}
	}
	return nil
}

// sieveQuota charges count and storage; checkSieveContent has judged the size.
func (s *Server) sieveQuota(ctx context.Context, h *userHandle, user, name string, size int) *jmapcore.SetError {
	err := sieve.CheckQuota(ctx, s.opts.Sieve.Store, user, h.info.Home, name, size, s.opts.Sieve.Quota)
	var qe *sieve.QuotaExceededError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &qe):
		return &jmapcore.SetError{Type: jmapcore.SetErrOverQuota, Description: qe.Error()}
	}
	return &jmapcore.SetError{Type: jmapcore.SetErrForbidden, Description: "the quota could not be checked"}
}

func (s *Server) sieveBlob(ctx context.Context, h *userHandle, user, blobID string) ([]byte, *jmapcore.SetError) {
	src, found, err := s.scriptBlob(ctx, h, user, blobID)
	if err == nil && !found {
		src, err = s.readBlob(ctx, h, blobID)
	}
	if err != nil {
		return nil, &jmapcore.SetError{Type: "blobNotFound", NotFound: []string{blobID}}
	}
	return src, nil
}

// markUpdated adds a server-changed property to an updated entry.
func markUpdated(resp *jmapcore.SetResponse, id, prop string, val any) {
	m, _ := resp.Updated[id].(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	m[prop] = val
	resp.Updated[id] = m
}

// sieveSet implements SieveScript/set (RFC 9661 §2.4).
func (s *Server) sieveSet(ctx context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req sieveSetRequest
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
	v, err := s.sieveRead(ctx, h, h.user)
	if err != nil {
		return nil, storeFailure("SieveScript/set state", accountID, err)
	}
	if req.IfInState != nil && *req.IfInState != v.state {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrStateMismatch}
	}
	resp := jmapcore.NewSetResponse(accountID, v.state)
	st, home := s.opts.Sieve.Store, h.info.Home

	for _, cid := range sortedKeys(req.Create) {
		created, serr := s.sieveCreate(ctx, h, h.user, &v, req.Create[cid])
		if serr != nil {
			resp.NotCreated[cid] = serr
			continue
		}
		resp.Created[cid] = created
		noteCreated(ctx, cid, created)
	}
	for _, id := range sortedKeys(req.Update) {
		updated, serr := s.sieveUpdate(ctx, h, h.user, &v, id, req.Update[id])
		if serr != nil {
			resp.NotUpdated[id] = serr
			continue
		}
		resp.Updated[id] = updated
	}
	for _, id := range req.Destroy {
		name, ok := v.ix.NameOf(id)
		switch {
		case !ok:
			resp.NotDestroyed[id] = &jmapcore.SetError{Type: jmapcore.SetErrNotFound}
			continue
		case name == v.active:
			resp.NotDestroyed[id] = &jmapcore.SetError{Type: jmapcore.SetErrSieveIsActive}
			continue
		}
		if err := st.DeleteScript(ctx, h.user, home, name); err != nil {
			return nil, storeFailure("SieveScript/set destroy", accountID, err)
		}
		delete(v.ix.IDs, name)
		resp.Destroyed = append(resp.Destroyed, id)
	}

	if len(resp.NotCreated)+len(resp.NotUpdated)+len(resp.NotDestroyed) == 0 {
		if merr := s.sieveActivation(ctx, h.user, home, &v, &req, resp); merr != nil {
			return nil, merr
		}
	}
	if after, err := st.Index(ctx, h.user, home); err == nil {
		resp.NewState = strconv.FormatUint(after.State, 10)
	}
	return resp, nil
}

func (s *Server) sieveCreate(ctx context.Context, h *userHandle, user string, v *sieveView, raw json.RawMessage) (map[string]any, *jmapcore.SetError) {
	p, serr := readSieveProps(raw)
	if serr != nil {
		return nil, serr
	}
	if p.BlobID == nil {
		return nil, invalidProp("blobId", "blobId is required")
	}
	src, serr := s.sieveBlob(ctx, h, user, *p.BlobID)
	if serr != nil {
		return nil, serr
	}
	if serr := s.checkSieveContent(src); serr != nil {
		return nil, serr
	}
	out := map[string]any{"isActive": false, "blobId": scriptBlobID(src)}
	name := ""
	if p.Name != nil {
		name = *p.Name
	} else {
		name = "script-" + scriptBlobID(src)[1:13]
		out["name"] = name
	}
	if serr := s.checkSieveName(*v, name); serr != nil {
		return nil, serr
	}
	if serr := s.sieveQuota(ctx, h, user, name, len(src)); serr != nil {
		return nil, serr
	}
	st := s.opts.Sieve.Store
	if err := st.CreateScript(ctx, user, h.info.Home, name, src); errors.Is(err, sieve.ErrScriptExists) {
		return nil, s.taken(ctx, user, h.info.Home, name, v)
	} else if err != nil {
		return nil, &jmapcore.SetError{Type: jmapcore.SetErrForbidden, Description: "the script could not be stored"}
	}
	ix, err := st.Index(ctx, user, h.info.Home)
	if err != nil {
		return nil, &jmapcore.SetError{Type: jmapcore.SetErrForbidden, Description: "the script was stored but has no id"}
	}
	v.ix = ix
	out["id"] = ix.IDs[name]
	return out, nil
}

func (s *Server) sieveUpdate(ctx context.Context, h *userHandle, user string, v *sieveView, id string, raw json.RawMessage) (any, *jmapcore.SetError) {
	name, ok := v.ix.NameOf(id)
	if !ok {
		return nil, &jmapcore.SetError{Type: jmapcore.SetErrNotFound}
	}
	p, serr := readSieveProps(raw)
	if serr != nil {
		return nil, serr
	}
	st, home := s.opts.Sieve.Store, h.info.Home
	var src []byte
	if p.BlobID != nil {
		if src, serr = s.sieveBlob(ctx, h, user, *p.BlobID); serr != nil {
			return nil, serr
		}
		if serr := s.checkSieveContent(src); serr != nil {
			return nil, serr
		}
		if serr := s.sieveQuota(ctx, h, user, name, len(src)); serr != nil {
			return nil, serr
		}
	}
	rename := p.Name != nil && *p.Name != name
	if rename {
		if serr := s.checkSieveName(*v, *p.Name); serr != nil {
			return nil, serr
		}
		// Renamed before the content is written: a refused name leaves nothing half done.
		if err := st.RenameScript(ctx, user, home, name, *p.Name); errors.Is(err, sieve.ErrScriptExists) {
			return nil, s.taken(ctx, user, home, *p.Name, v)
		} else if err != nil {
			return nil, &jmapcore.SetError{Type: jmapcore.SetErrForbidden, Description: "the script could not be renamed"}
		}
		delete(v.ix.IDs, name)
		v.ix.IDs[*p.Name] = id
		if v.active == name {
			v.active = *p.Name
		}
		name = *p.Name
	}
	if src == nil {
		return nil, nil
	}
	if err := st.SaveScript(ctx, user, home, name, src); err != nil {
		return nil, &jmapcore.SetError{Type: jmapcore.SetErrForbidden, Description: "the script could not be stored"}
	}
	return map[string]any{"blobId": scriptBlobID(src)}, nil
}

// sieveActivation applies onSuccessDeactivateScript, then
// onSuccessActivateScript; an id that names no script is ignored (§2.4).
func (s *Server) sieveActivation(ctx context.Context, user, home string, v *sieveView, req *sieveSetRequest, resp *jmapcore.SetResponse) *jmapcore.MethodError {
	st := s.opts.Sieve.Store
	report := func(id string, on bool) {
		for cid, c := range resp.Created {
			if m, ok := c.(map[string]any); ok && m["id"] == id {
				m["isActive"] = on
				resp.Created[cid] = m
				return
			}
		}
		markUpdated(resp, id, "isActive", on)
	}
	if req.OnSuccessDeactivateScript && v.active != "" {
		if err := st.Deactivate(ctx, user, home); err != nil {
			return storeFailure("SieveScript/set deactivate", user, err)
		}
		report(v.ix.IDs[v.active], false)
		v.active = ""
	}
	if req.OnSuccessActivateScript == nil {
		return nil
	}
	target := jmapcore.ResolveID(ctx, *req.OnSuccessActivateScript)
	name, ok := v.ix.NameOf(target)
	if !ok || name == v.active {
		return nil
	}
	if err := st.SetActive(ctx, user, home, name); err != nil {
		return storeFailure("SieveScript/set activate", user, err)
	}
	if v.active != "" {
		report(v.ix.IDs[v.active], false)
	}
	report(target, true)
	v.active = name
	return nil
}

type sieveFilter struct {
	Name     *string `json:"name"`
	IsActive *bool   `json:"isActive"`
}

// sieveQuery implements SieveScript/query (RFC 9661 §2.5).
func (s *Server) sieveQuery(ctx context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
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
	var f sieveFilter
	for k := range req.Filter {
		if k != "name" && k != "isActive" {
			return nil, &jmapcore.MethodError{Type: jmapcore.ErrUnsupportedFilter, Description: fmt.Sprintf("cannot filter on %q", k)}
		}
	}
	if req.Filter != nil {
		raw, _ := json.Marshal(req.Filter)
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
		}
	}
	for _, c := range req.Sort {
		if c.Property != "name" && c.Property != "isActive" {
			return nil, &jmapcore.MethodError{Type: jmapcore.ErrUnsupportedSort, Description: fmt.Sprintf("cannot sort on %q", c.Property)}
		}
	}
	v, err := s.sieveRead(ctx, h, h.user)
	if err != nil {
		return nil, storeFailure("SieveScript/query failed", accountID, err)
	}
	matched := []jmapcore.SieveScript{}
	for _, sc := range v.scripts {
		if f.Name != nil && !strings.Contains(sc.Name, *f.Name) {
			continue
		}
		if f.IsActive != nil && sc.IsActive != *f.IsActive {
			continue
		}
		matched = append(matched, sc)
	}
	sort.SliceStable(matched, func(i, j int) bool {
		for _, c := range req.Sort {
			a, b := sieveSortKey(matched[i], c.Property), sieveSortKey(matched[j], c.Property)
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
	for _, sc := range matched {
		ids = append(ids, sc.ID)
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
	resp := jmapcore.QueryResponse{AccountID: accountID, QueryState: v.state, Position: pos, IDs: ids, Limit: req.Limit}
	if req.CalculateTotal {
		resp.Total = &total
	}
	return resp, nil
}

func sieveSortKey(sc jmapcore.SieveScript, prop string) string {
	if prop == "isActive" {
		return strconv.FormatBool(sc.IsActive)
	}
	return sc.Name
}

// sieveValidate implements SieveScript/validate (RFC 9661 §2.6).
func (s *Server) sieveValidate(ctx context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req struct {
		AccountID string `json:"accountId"`
		BlobID    string `json:"blobId"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	src, serr := s.sieveBlob(ctx, h, h.user, req.BlobID)
	if serr != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: "no such blob: " + req.BlobID}
	}
	var verr *jmapcore.SetError
	if e := s.checkSieveContent(src); e != nil && e.Type == jmapcore.SetErrInvalidSieve {
		verr = e
	}
	return map[string]any{"accountId": accountID, "error": verr}, nil
}
