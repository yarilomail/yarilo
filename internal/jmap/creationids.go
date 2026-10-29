package jmap

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/yarilomail/yarilo/v2/pkg/jmapcore"
)

// noteCreated records a create's id for later "#<creation id>" values (RFC 8620 §5.3).
func noteCreated(ctx context.Context, cid string, created any) {
	if m, ok := created.(map[string]any); ok {
		if id, ok := m["id"].(string); ok {
			jmapcore.AddCreated(ctx, cid, id)
		}
	}
}

func resolveGet(ctx context.Context, req *jmapcore.GetRequest) {
	if req.IDs != nil {
		jmapcore.ResolveIDs(ctx, *req.IDs)
	}
}

func resolveSet(ctx context.Context, req *jmapcore.SetRequest) {
	req.Update = jmapcore.ResolveKeys(ctx, req.Update)
	jmapcore.ResolveIDs(ctx, req.Destroy)
}

func resolveEmailFilter(ctx context.Context, f *jmapcore.EmailFilter) {
	if f == nil {
		return
	}
	if f.InMailbox != nil {
		id := jmapcore.ResolveID(ctx, *f.InMailbox)
		f.InMailbox = &id
	}
	jmapcore.ResolveIDs(ctx, f.InMailboxOtherThan)
}

// resolveEmailPatch resolves the mailbox ids an Email/set update names, as
// "mailboxIds" keys or in "mailboxIds/<id>" paths.
func resolveEmailPatch(ctx context.Context, raw json.RawMessage) json.RawMessage {
	var patch map[string]json.RawMessage
	if json.Unmarshal(raw, &patch) != nil {
		return raw
	}
	out := make(map[string]json.RawMessage, len(patch))
	for name, v := range patch {
		if id, ok := strings.CutPrefix(name, "mailboxIds/"); ok {
			name = "mailboxIds/" + jmapcore.ResolveID(ctx, id)
		} else if name == "mailboxIds" {
			var set map[string]bool
			if json.Unmarshal(v, &set) == nil {
				v, _ = json.Marshal(jmapcore.ResolveKeys(ctx, set))
			}
		}
		out[name] = v
	}
	b, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	return b
}

// resolveProperty resolves the id-valued property name of one create or
// update object; a value that is not a string is left for the method to refuse.
func resolveProperty(ctx context.Context, raw json.RawMessage, name string) json.RawMessage {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return raw
	}
	var id string
	if v := obj[name]; len(v) == 0 || v[0] != '"' || json.Unmarshal(v, &id) != nil {
		return raw
	}
	obj[name], _ = json.Marshal(jmapcore.ResolveID(ctx, id))
	b, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return b
}
