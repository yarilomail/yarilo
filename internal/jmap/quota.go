package jmap

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
	"github.com/yarilomail/yarilo/pkg/mailbox"
	"github.com/yarilomail/yarilo/pkg/quota"
)

// quotaRegistry binds the Quota methods of RFC 9425 for one authenticated user.
func (s *Server) quotaRegistry(lazy *lazyStore, accountID string) jmapcore.Registry {
	return jmapcore.Registry{
		"Quota/get": {Capability: jmapcore.CapQuota, Fn: func(ctx context.Context, args json.RawMessage) (any, *jmapcore.MethodError) {
			return s.withStore(lazy, accountID, func(h *userHandle) (any, *jmapcore.MethodError) {
				return s.quotaGet(ctx, h, accountID, args)
			})
		}},
		"Quota/changes": {Capability: jmapcore.CapQuota, Fn: func(ctx context.Context, args json.RawMessage) (any, *jmapcore.MethodError) {
			return s.withStore(lazy, accountID, func(h *userHandle) (any, *jmapcore.MethodError) {
				return s.quotaChanges(ctx, h, accountID, args)
			})
		}},
		"Quota/query": {Capability: jmapcore.CapQuota, Fn: func(ctx context.Context, args json.RawMessage) (any, *jmapcore.MethodError) {
			return s.withStore(lazy, accountID, func(h *userHandle) (any, *jmapcore.MethodError) {
				return s.quotaQuery(ctx, h, accountID, args)
			})
		}},
		"Quota/queryChanges": {Capability: jmapcore.CapQuota, Fn: func(ctx context.Context, args json.RawMessage) (any, *jmapcore.MethodError) {
			return s.withStore(lazy, accountID, func(h *userHandle) (any, *jmapcore.MethodError) {
				return s.quotaQueryChanges(ctx, h, accountID, args)
			})
		}},
	}
}

// quotaObjects answers the account's Quota objects and the state they are in.
// The numbers come from the count IMAP GETQUOTA reads, because a second
// accounting is a second answer to one question (#1856).
func (s *Server) quotaObjects(h *userHandle) ([]jmapcore.Quota, string, error) {
	limits := s.opts.QuotaPolicy.Scale(quotaRulesOf(h))
	entries, err := h.box.ListFolders()
	if err != nil {
		return nil, "", err
	}
	used := quota.CountUsage(h.mbox, h.idx, mailbox.SelectableNames(entries), limits)
	state := quotaStateOf(used, limits)
	// The gates IMAP answers on, plus the engine itself: a limit nobody
	// enforces is not a limit, and reporting zero would read as "no headroom".
	if !s.opts.QuotaEnabled || s.opts.QuotaPolicy.Hidden || (limits.Unlimited() && s.opts.QuotaPolicy.IgnoreUnlimited) {
		return nil, state, nil
	}
	var out []jmapcore.Quota
	if limits.StorageBytes > 0 {
		out = append(out, s.quotaObject(jmapcore.QuotaResourceOctets, used.StorageBytes, limits.StorageBytes, "storage", limits))
	}
	if limits.Messages > 0 {
		out = append(out, s.quotaObject(jmapcore.QuotaResourceCount, used.Messages, limits.Messages, "message", limits))
	}
	return out, state, nil
}

// quotaObject builds one Quota. The id is the resource type: there is one root
// per account, so a longer id would carry no more information.
func (s *Server) quotaObject(resource string, used, hard int64, warnResource string, limits quota.Limits) jmapcore.Quota {
	q := jmapcore.Quota{
		ID:           resource,
		ResourceType: resource,
		Used:         used,
		HardLimit:    hard,
		Scope:        jmapcore.QuotaScopeAccount,
		Name:         s.quotaName(),
		Types:        []string{jmapcore.QuotaTypeMail},
	}
	if warn, ok := lowestWarnLimit(s.opts.QuotaPolicy.Warnings, warnResource, limits); ok {
		q.WarnLimit = &warn
	}
	return q
}

// lowestWarnLimit is the nearest quota_warning threshold on this resource, as
// bytes or messages. A client shows the first warning it will meet, not all.
func lowestWarnLimit(warnings []quota.Warning, resource string, limits quota.Limits) (int64, bool) {
	limit := limits.StorageBytes
	if resource == "message" {
		limit = limits.Messages
	}
	if limit <= 0 {
		return 0, false
	}
	best, found := int64(0), false
	for _, w := range warnings {
		if !sameResource(w.Resource, resource) || !strings.EqualFold(warnThreshold(w), "over") {
			continue
		}
		pct := w.Percentage
		if pct <= 0 {
			pct = 100
		}
		at := limit * int64(pct) / 100
		if at <= 0 || at >= limit {
			continue
		}
		if !found || at < best {
			best, found = at, true
		}
	}
	return best, found
}

func sameResource(configured, want string) bool {
	if configured == "" {
		configured = "storage"
	}
	return strings.EqualFold(configured, want)
}

func warnThreshold(w quota.Warning) string {
	if w.Threshold == "" {
		return "over"
	}
	return w.Threshold
}

func (s *Server) quotaName() string {
	if n := s.opts.QuotaName; n != "" {
		return n
	}
	return quota.RootName
}

func quotaRulesOf(h *userHandle) quota.Limits {
	if h.info == nil {
		return quota.Limits{}
	}
	return quota.ParseRules(h.info.QuotaRules)
}

// quotaStateOf carries the numbers themselves, so Quota/changes says which
// object moved without keeping per-client history of what it last saw.
func quotaStateOf(used quota.Usage, limits quota.Limits) string {
	return fmt.Sprintf("q-%d-%d-%d-%d", used.StorageBytes, used.Messages, limits.StorageBytes, limits.Messages)
}

func (s *Server) quotaGet(_ context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req jmapcore.GetRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	all, state, err := s.quotaObjects(h)
	if err != nil {
		return nil, storeFailure("Quota/get failed", accountID, err)
	}
	resp := &jmapcore.GetResponse[jmapcore.Quota]{
		AccountID: accountID, State: state,
		List: []jmapcore.Quota{}, NotFound: []string{},
	}
	if req.IDs == nil {
		resp.List = append(resp.List, all...)
		return resp, nil
	}
	byID := map[string]jmapcore.Quota{}
	for _, q := range all {
		byID[q.ID] = q
	}
	for _, id := range *req.IDs {
		if q, ok := byID[id]; ok {
			resp.List = append(resp.List, q)
			continue
		}
		resp.NotFound = append(resp.NotFound, id)
	}
	return resp, nil
}

// quotaChanges names the objects whose numbers moved since sinceState. The
// state carries them, so the comparison needs nothing kept between requests.
func (s *Server) quotaChanges(_ context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req jmapcore.ChangesRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	if req.SinceState == "" {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: "sinceState is required"}
	}
	oldUsed, oldLimits, ok := parseQuotaState(req.SinceState)
	if !ok {
		return nil, cannotCalculate("sinceState is not a state this server issued")
	}
	all, state, err := s.quotaObjects(h)
	if err != nil {
		return nil, storeFailure("Quota/changes failed", accountID, err)
	}
	resp := &jmapcore.ChangesResponse{
		AccountID: accountID, OldState: req.SinceState, NewState: state,
		Created: []string{}, Updated: []string{}, Destroyed: []string{},
	}
	for _, q := range all {
		before, hadLimit := int64(0), false
		switch q.ResourceType {
		case jmapcore.QuotaResourceOctets:
			before, hadLimit = oldUsed.StorageBytes, oldLimits.StorageBytes > 0
			if q.Used != before || q.HardLimit != oldLimits.StorageBytes {
				resp.Updated = append(resp.Updated, q.ID)
			}
		case jmapcore.QuotaResourceCount:
			before, hadLimit = oldUsed.Messages, oldLimits.Messages > 0
			if q.Used != before || q.HardLimit != oldLimits.Messages {
				resp.Updated = append(resp.Updated, q.ID)
			}
		}
		if !hadLimit {
			// The limit was absent then and is here now: to a client tracking
			// state the object is new, not changed.
			resp.Created = append(resp.Created, q.ID)
			resp.Updated = trimID(resp.Updated, q.ID)
		}
	}
	resp.Destroyed = append(resp.Destroyed, quotaIDsGone(all, oldLimits)...)
	return resp, nil
}

// quotaIDsGone names the objects the old state had and this one does not.
func quotaIDsGone(now []jmapcore.Quota, oldLimits quota.Limits) []string {
	present := map[string]bool{}
	for _, q := range now {
		present[q.ID] = true
	}
	var gone []string
	if oldLimits.StorageBytes > 0 && !present[jmapcore.QuotaResourceOctets] {
		gone = append(gone, jmapcore.QuotaResourceOctets)
	}
	if oldLimits.Messages > 0 && !present[jmapcore.QuotaResourceCount] {
		gone = append(gone, jmapcore.QuotaResourceCount)
	}
	return gone
}

func trimID(ids []string, drop string) []string {
	out := ids[:0]
	for _, id := range ids {
		if id != drop {
			out = append(out, id)
		}
	}
	return out
}

func parseQuotaState(state string) (quota.Usage, quota.Limits, bool) {
	var usedB, usedM, limB, limM int64
	n, err := fmt.Sscanf(state, "q-%d-%d-%d-%d", &usedB, &usedM, &limB, &limM)
	if err != nil || n != 4 {
		return quota.Usage{}, quota.Limits{}, false
	}
	return quota.Usage{StorageBytes: usedB, Messages: usedM}, quota.Limits{StorageBytes: limB, Messages: limM}, true
}

func (s *Server) quotaQuery(_ context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req struct {
		jmapcore.QueryRequest
		Filter *struct {
			Name         *string  `json:"name"`
			Scope        *string  `json:"scope"`
			ResourceType *string  `json:"resourceType"`
			Type         *string  `json:"type"`
			Types        []string `json:"types"`
		} `json:"filter"`
		Sort []jmapcore.Comparator `json:"sort"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	all, state, err := s.quotaObjects(h)
	if err != nil {
		return nil, storeFailure("Quota/query failed", accountID, err)
	}
	ids := []string{}
	for _, q := range all {
		if req.Filter != nil {
			if req.Filter.Name != nil && *req.Filter.Name != q.Name {
				continue
			}
			if req.Filter.Scope != nil && *req.Filter.Scope != q.Scope {
				continue
			}
			if req.Filter.ResourceType != nil && *req.Filter.ResourceType != q.ResourceType {
				continue
			}
			if req.Filter.Type != nil && !hasType(q, *req.Filter.Type) {
				continue
			}
		}
		ids = append(ids, q.ID)
	}
	sort.Strings(ids)
	return &jmapcore.QueryResponse{
		AccountID: accountID, QueryState: state,
		// The result set is the account's quota roots: it is derivable from the
		// state, which is what lets queryChanges answer instead of refusing.
		CanCalculateChanges: true,
		Position:            0,
		IDs:                 ids,
	}, nil
}

func hasType(q jmapcore.Quota, want string) bool {
	for _, t := range q.Types {
		if t == want {
			return true
		}
	}
	return false
}

// quotaQueryChanges answers rather than refusing, unlike the Mailbox and Email
// ones: this result set is the account's roots, which the state already names.
func (s *Server) quotaQueryChanges(ctx context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req struct {
		AccountID       string `json:"accountId"`
		SinceQueryState string `json:"sinceQueryState"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	if req.SinceQueryState == "" {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: "sinceQueryState is required"}
	}
	_, oldLimits, ok := parseQuotaState(req.SinceQueryState)
	if !ok {
		return nil, cannotCalculate("sinceQueryState is not a state this server issued")
	}
	all, state, err := s.quotaObjects(h)
	if err != nil {
		return nil, storeFailure("Quota/queryChanges failed", accountID, err)
	}
	_ = ctx
	added := []jmapcore.AddedItem{}
	removed := quotaIDsGone(all, oldLimits)
	if removed == nil {
		removed = []string{}
	}
	for i, q := range all {
		had := (q.ResourceType == jmapcore.QuotaResourceOctets && oldLimits.StorageBytes > 0) ||
			(q.ResourceType == jmapcore.QuotaResourceCount && oldLimits.Messages > 0)
		if !had {
			added = append(added, jmapcore.AddedItem{ID: q.ID, Index: uint(i)})
		}
	}
	return &jmapcore.QueryChangesResponse{
		AccountID: accountID, OldQueryState: req.SinceQueryState, NewQueryState: state,
		Removed: removed, Added: added,
	}, nil
}
