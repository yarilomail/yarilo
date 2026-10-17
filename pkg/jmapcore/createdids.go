package jmapcore

import (
	"context"
	"strings"
)

// createdIDs maps the creation ids of one request to the ids they created
// (RFC 8620 §5.3), seeded from the request's createdIds (§3.3).
type createdIDs map[string]string

type createdIDsKey struct{}

func withCreatedIDs(ctx context.Context, m createdIDs) context.Context {
	return context.WithValue(ctx, createdIDsKey{}, m)
}

// AddCreated records what a creation id created; a reused creation id maps to
// the most recently created item (RFC 8620 §5.3).
func AddCreated(ctx context.Context, creationID, id string) {
	if m, ok := ctx.Value(createdIDsKey{}).(createdIDs); ok {
		m[creationID] = id
	}
}

// ResolveID turns "#<creation id>" into the id created earlier in the request;
// anything else, an unknown creation id included, comes back as given.
func ResolveID(ctx context.Context, id string) string {
	cid, ok := strings.CutPrefix(id, "#")
	if !ok {
		return id
	}
	if m, ok := ctx.Value(createdIDsKey{}).(createdIDs); ok {
		if v, ok := m[cid]; ok {
			return v
		}
	}
	return id
}

// ResolveIDs applies ResolveID to each id, in place.
func ResolveIDs(ctx context.Context, ids []string) []string {
	for i, id := range ids {
		ids[i] = ResolveID(ctx, id)
	}
	return ids
}

// ResolveKeys applies ResolveID to the keys of an id-keyed map.
func ResolveKeys[V any](ctx context.Context, m map[string]V) map[string]V {
	if m == nil {
		return nil
	}
	out := make(map[string]V, len(m))
	for k, v := range m {
		out[ResolveID(ctx, k)] = v
	}
	return out
}
