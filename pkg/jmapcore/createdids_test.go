package jmapcore

import (
	"context"
	"encoding/json"
	"testing"
)

// thingRegistry creates "b" as T2 and answers Thing/get with what "#a" and
// "#b" resolve to.
func thingRegistry() Registry {
	reg := CoreRegistry()
	reg["Thing/set"] = MethodEntry{Capability: CapCore, Fn: func(ctx context.Context, _ json.RawMessage) (any, *MethodError) {
		AddCreated(ctx, "b", "T2")
		return map[string]string{}, nil
	}}
	reg["Thing/get"] = MethodEntry{Capability: CapCore, Fn: func(ctx context.Context, _ json.RawMessage) (any, *MethodError) {
		return []string{ResolveID(ctx, "#a"), ResolveID(ctx, "#b"), ResolveID(ctx, "#nope"), ResolveID(ctx, "T9")}, nil
	}}
	return reg
}

func TestCreatedIDs(t *testing.T) {
	cases := []struct {
		name, created string // the request's createdIds, "" for absent
		wantGet       string
		wantCreated   string // the response's createdIds, "" for absent
	}{
		{"the request's map is extended and returned", `{"a":"M1"}`, `["M1","T2","#nope","T9"]`, `{"a":"M1","b":"T2"}`},
		{"an empty map comes back filled", `{}`, `["#a","T2","#nope","T9"]`, `{"b":"T2"}`},
		{"none sent, none returned", "", `["#a","T2","#nope","T9"]`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := &Request{Using: []string{CapCore}, MethodCalls: []Invocation{
				{Name: "Thing/set", Args: json.RawMessage(`{}`), CallID: "c0"},
				{Name: "Thing/get", Args: json.RawMessage(`{}`), CallID: "c1"},
			}}
			if c.created != "" {
				if err := json.Unmarshal([]byte(c.created), &req.CreatedIDs); err != nil {
					t.Fatal(err)
				}
			}
			resp := Execute(context.Background(), req, thingRegistry(), testLimits())
			if got := string(resp.MethodResponses[1].Args); got != c.wantGet {
				t.Errorf("resolved %s, want %s", got, c.wantGet)
			}
			out, _ := json.Marshal(resp)
			var env map[string]json.RawMessage
			_ = json.Unmarshal(out, &env)
			if got := string(env["createdIds"]); got != c.wantCreated {
				t.Errorf("createdIds = %q, want %q", got, c.wantCreated)
			}
		})
	}
}

func TestAddCreatedOutsideARequestIsIgnored(t *testing.T) {
	AddCreated(context.Background(), "a", "X")
	if got := ResolveID(context.Background(), "#a"); got != "#a" {
		t.Errorf("ResolveID = %q outside a request, want it as given", got)
	}
}
