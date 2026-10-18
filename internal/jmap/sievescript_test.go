package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/sieve"
	"github.com/yarilomail/yarilo/pkg/jmapcore"
)

// sieveServer serves testUser with RFC 9661 over a fresh fs script store.
func sieveServer(t *testing.T, q sieve.Quota, exts ...string) (*Server, string, *sieve.IndexedStore) {
	t.Helper()
	s, _, home := storedServerWithMessageAt(t, "Subject: seed\r\n\r\nseed\r\n", 0)
	st := sieve.NewScriptStore("fs", "", nil, nil)
	s.opts.Sieve = &SieveScripts{Store: st, Quota: q, Extensions: exts, MaxRedirects: 32, Implementation: "yarilo test"}
	s.opts.Limits.Sieve = s.opts.Sieve.session()
	s.opts.Limits.MaxSizeUpload = 4096
	s.opts.UploadTTL = time.Hour
	return s, home, st
}

func sieveCall(t *testing.T, s *Server, method, args string) map[string]any {
	t.Helper()
	name, out := callAs(t, s, testUser, method, args)
	if name != method {
		t.Fatalf("%s answered %s %v", method, name, out)
	}
	return out
}

func entry(out map[string]any, bucket, key string) map[string]any {
	m, _ := out[bucket].(map[string]any)
	e, _ := m[key].(map[string]any)
	return e
}

const (
	keepScript    = "keep;\n"
	discardScript = "discard;\n"
)

func createArgs(blob, name, extra string) string {
	nameField := ""
	if name != "" {
		nameField = `,"name":"` + name + `"`
	}
	return `{"accountId":"` + testUser + `","create":{"c1":{"blobId":"` + blob + `"` + nameField + `}}` + extra + `}`
}

func TestSieveScriptCreate(t *testing.T) {
	s, home, st := sieveServer(t, sieve.Quota{})
	out := sieveCall(t, s, "SieveScript/set", createArgs(uploadAs(t, s, testUser, keepScript), "mine", ""))
	c := entry(out, "created", "c1")
	if c["id"] == nil || c["isActive"] != false || !strings.HasPrefix(fmt.Sprint(c["blobId"]), "V") {
		t.Fatalf("created = %v, want an id, isActive false and a script blob id", c)
	}
	// The store ManageSieve opens holds it under the same name and bytes.
	src, found, err := st.GetScript(context.Background(), testUser, home, "mine")
	if err != nil || !found || string(src) != keepScript {
		t.Errorf("ManageSieve's store has %q (found %v, err %v), want %q", src, found, err, keepScript)
	}
	if out["oldState"] == out["newState"] {
		t.Errorf("state did not move: %v", out)
	}
}

func TestSieveScriptCreateWithoutAName(t *testing.T) {
	s, home, st := sieveServer(t, sieve.Quota{})
	out := sieveCall(t, s, "SieveScript/set", createArgs(uploadAs(t, s, testUser, keepScript), "", ""))
	name, _ := entry(out, "created", "c1")["name"].(string)
	if name == "" {
		t.Fatalf("created = %v, want the name the server chose", out)
	}
	if _, found, _ := st.GetScript(context.Background(), testUser, home, name); !found {
		t.Errorf("no script %q in the store", name)
	}
}

// A script stored over ManageSieve is listed, and moves the state.
func TestSieveScriptSeesManageSieveWrites(t *testing.T) {
	s, home, st := sieveServer(t, sieve.Quota{})
	before := sieveCall(t, s, "SieveScript/get", `{"accountId":"`+testUser+`","ids":null}`)
	if err := st.SaveScript(context.Background(), testUser, home, "from-managesieve", []byte(keepScript)); err != nil {
		t.Fatal(err)
	}
	after := sieveCall(t, s, "SieveScript/get", `{"accountId":"`+testUser+`","ids":null}`)
	list, _ := after["list"].([]any)
	if len(list) != 1 || !strings.Contains(fmt.Sprint(list[0]), "from-managesieve") {
		t.Errorf("list = %v, want the ManageSieve script", list)
	}
	if before["state"] == after["state"] {
		t.Errorf("state %v did not move", after["state"])
	}
}

func TestSieveScriptRefusals(t *testing.T) {
	cases := []struct {
		name   string
		q      sieve.Quota
		exts   []string
		seed   []string
		script string
		as     string // name; "" is a fixed valid one
		want   string
		check  func(t *testing.T, e map[string]any)
	}{
		{name: "not Sieve", script: "if true {", want: "invalidSieve"},
		{name: "not UTF-8", script: "keep; # \xff\n", want: "invalidSieve"},
		{name: "an extension not allowed", exts: []string{"fileinto"}, script: "require \"vacation\";\nkeep;\n", want: "invalidSieve"},
		{name: "over the size", q: sieve.Quota{MaxSize: 5}, script: keepScript, want: "tooLarge"},
		{name: "past the count", q: sieve.Quota{MaxScripts: 1}, seed: []string{"a"}, script: keepScript, want: "overQuota"},
		{name: "past the storage", q: sieve.Quota{MaxStorage: 10}, seed: []string{"a"}, script: keepScript, want: "overQuota"},
		{name: "the reserved name", script: keepScript, as: "yarilo", want: "invalidProperties"},
		{name: "a name over 512 octets", script: keepScript, as: strings.Repeat("€", 200), want: "invalidProperties"},
		{name: "a taken name", seed: []string{"a"}, script: keepScript, as: "a", want: "alreadyExists",
			check: func(t *testing.T, e map[string]any) {
				if e["existingId"] != "S1" {
					t.Errorf("existingId = %v, want S1", e["existingId"])
				}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, home, st := sieveServer(t, c.q, c.exts...)
			for _, n := range c.seed {
				if err := st.SaveScript(context.Background(), testUser, home, n, []byte(keepScript)); err != nil {
					t.Fatal(err)
				}
			}
			as := c.as
			if as == "" {
				as = "new"
			}
			out := sieveCall(t, s, "SieveScript/set", createArgs(uploadAs(t, s, testUser, c.script), as, ""))
			e := entry(out, "notCreated", "c1")
			if e["type"] != c.want {
				t.Fatalf("notCreated c1 = %v, want %s (response %v)", e, c.want, out)
			}
			if c.check != nil {
				c.check(t, e)
			}
			if names, _ := st.ListScripts(context.Background(), testUser, home); len(names) != len(c.seed) {
				t.Errorf("store holds %v after a refusal", names)
			}
		})
	}
}

func TestSieveScriptActivation(t *testing.T) {
	ctx := context.Background()
	s, home, st := sieveServer(t, sieve.Quota{})
	blob := uploadAs(t, s, testUser, keepScript)

	out := sieveCall(t, s, "SieveScript/set", createArgs(blob, "one", `,"onSuccessActivateScript":"#c1"`))
	one, _ := entry(out, "created", "c1")["id"].(string)
	if entry(out, "created", "c1")["isActive"] != true {
		t.Fatalf("created = %v, want isActive true", out["created"])
	}
	if active, _ := st.ActiveScriptName(ctx, testUser, home); active != "one" {
		t.Fatalf("active = %q, want one", active)
	}

	out = sieveCall(t, s, "SieveScript/set", createArgs(blob, "two", `,"onSuccessActivateScript":"#c1"`))
	if entry(out, "updated", one)["isActive"] != false {
		t.Errorf("updated = %v, want %s reported inactive", out["updated"], one)
	}
	two, _ := entry(out, "created", "c1")["id"].(string)

	out = sieveCall(t, s, "SieveScript/set", `{"accountId":"`+testUser+`","destroy":["`+two+`"]}`)
	if entry(out, "notDestroyed", two)["type"] != "sieveIsActive" {
		t.Errorf("destroying the active script = %v, want sieveIsActive", out)
	}

	out = sieveCall(t, s, "SieveScript/set", `{"accountId":"`+testUser+`","onSuccessDeactivateScript":true}`)
	if entry(out, "updated", two)["isActive"] != false {
		t.Errorf("updated = %v, want %s reported inactive", out["updated"], two)
	}
	if active, _ := st.ActiveScriptName(ctx, testUser, home); active != "" {
		t.Errorf("active = %q after deactivation", active)
	}

	out = sieveCall(t, s, "SieveScript/set", `{"accountId":"`+testUser+`","destroy":["`+two+`"]}`)
	if d, _ := out["destroyed"].([]any); len(d) != 1 {
		t.Errorf("destroyed = %v, want %s", out["destroyed"], two)
	}
}

// Deactivation runs before activation: both in one call leave the named
// script active.
func TestSieveScriptDeactivateThenActivate(t *testing.T) {
	ctx := context.Background()
	s, home, st := sieveServer(t, sieve.Quota{})
	blob := uploadAs(t, s, testUser, keepScript)
	out := sieveCall(t, s, "SieveScript/set", createArgs(blob, "one", ""))
	one, _ := entry(out, "created", "c1")["id"].(string)
	sieveCall(t, s, "SieveScript/set", `{"accountId":"`+testUser+`","onSuccessDeactivateScript":true,"onSuccessActivateScript":"`+one+`"}`)
	if active, _ := st.ActiveScriptName(ctx, testUser, home); active != "one" {
		t.Errorf("active = %q, want one", active)
	}
}

// A failed object leaves the active script as it was.
func TestSieveScriptActivationNeedsEverySuccess(t *testing.T) {
	ctx := context.Background()
	s, home, st := sieveServer(t, sieve.Quota{})
	good := uploadAs(t, s, testUser, keepScript)
	bad := uploadAs(t, s, testUser, "if true {")
	args := `{"accountId":"` + testUser + `","create":{"c1":{"blobId":"` + good + `","name":"ok"},"c2":{"blobId":"` + bad + `","name":"bad"}},"onSuccessActivateScript":"#c1"}`
	out := sieveCall(t, s, "SieveScript/set", args)
	if entry(out, "created", "c1")["isActive"] != false {
		t.Errorf("created c1 = %v, want inactive", entry(out, "created", "c1"))
	}
	if active, _ := st.ActiveScriptName(ctx, testUser, home); active != "" {
		t.Errorf("active = %q, want none: c2 failed", active)
	}
}

func TestSieveScriptUpdate(t *testing.T) {
	ctx := context.Background()
	s, home, st := sieveServer(t, sieve.Quota{})
	out := sieveCall(t, s, "SieveScript/set", createArgs(uploadAs(t, s, testUser, keepScript), "old", `,"onSuccessActivateScript":"#c1"`))
	id, _ := entry(out, "created", "c1")["id"].(string)
	sieveCall(t, s, "SieveScript/set", createArgs(uploadAs(t, s, testUser, keepScript), "other", ""))

	out = sieveCall(t, s, "SieveScript/set", `{"accountId":"`+testUser+`","update":{"`+id+`":{"name":"other"}}}`)
	if e := entry(out, "notUpdated", id); e["type"] != "alreadyExists" || e["existingId"] == nil {
		t.Errorf("renaming onto a taken name = %v, want alreadyExists with existingId", e)
	}

	blob := uploadAs(t, s, testUser, discardScript)
	out = sieveCall(t, s, "SieveScript/set", `{"accountId":"`+testUser+`","update":{"`+id+`":{"name":"new","blobId":"`+blob+`"}}}`)
	if _, ok := out["updated"].(map[string]any)[id]; !ok {
		t.Fatalf("update = %v", out)
	}
	src, found, _ := st.GetScript(ctx, testUser, home, "new")
	if !found || string(src) != discardScript {
		t.Errorf("store has %q under new (found %v), want the new bytes", src, found)
	}
	if active, _ := st.ActiveScriptName(ctx, testUser, home); active != "new" {
		t.Errorf("active = %q, want the renamed script", active)
	}
	got := sieveCall(t, s, "SieveScript/get", `{"accountId":"`+testUser+`","ids":["`+id+`"]}`)
	if list, _ := got["list"].([]any); len(list) != 1 || !strings.Contains(fmt.Sprint(list[0]), "name:new") {
		t.Errorf("get %s = %v, want the same id under the new name", id, got)
	}

	// Renamed and deactivated in one call: the deactivation names the same id.
	out = sieveCall(t, s, "SieveScript/set", `{"accountId":"`+testUser+`","update":{"`+id+`":{"name":"newer"}},"onSuccessDeactivateScript":true}`)
	if entry(out, "updated", id)["isActive"] != false {
		t.Errorf("updated = %v, want %s reported inactive", out["updated"], id)
	}
}

func TestSieveScriptIfInState(t *testing.T) {
	s, _, _ := sieveServer(t, sieve.Quota{})
	name, out := callAs(t, s, testUser, "SieveScript/set", `{"accountId":"`+testUser+`","ifInState":"999","destroy":[]}`)
	if name != "error" || out["type"] != "stateMismatch" {
		t.Errorf("answered %s %v, want stateMismatch", name, out)
	}
}

func TestSieveScriptQuery(t *testing.T) {
	ctx := context.Background()
	s, home, st := sieveServer(t, sieve.Quota{})
	for _, n := range []string{"alpha", "beta", "gamma"} {
		_ = st.SaveScript(ctx, testUser, home, n, []byte(keepScript))
	}
	_ = st.SetActive(ctx, testUser, home, "beta")
	ix, _ := st.Index(ctx, testUser, home)
	cases := []struct {
		name, args string
		want       []string
	}{
		{"all by name", `"sort":[{"property":"name"}]`, []string{"alpha", "beta", "gamma"}},
		{"by name descending", `"sort":[{"property":"name","isAscending":false}]`, []string{"gamma", "beta", "alpha"}},
		{"active only", `"filter":{"isActive":true}`, []string{"beta"}},
		{"name contains", `"filter":{"name":"mm"}`, []string{"gamma"}},
		{"active first", `"sort":[{"property":"isActive","isAscending":false},{"property":"name"}]`, []string{"beta", "alpha", "gamma"}},
		{"a window", `"sort":[{"property":"name"}],"position":1,"limit":1`, []string{"beta"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := sieveCall(t, s, "SieveScript/query", `{"accountId":"`+testUser+`",`+c.args+`}`)
			var want []any
			for _, n := range c.want {
				want = append(want, ix.IDs[n])
			}
			if fmt.Sprint(out["ids"]) != fmt.Sprint(want) {
				t.Errorf("ids = %v, want %v", out["ids"], want)
			}
		})
	}
	for _, bad := range []string{`"filter":{"blobId":"x"}`, `"sort":[{"property":"blobId"}]`} {
		name, out := callAs(t, s, testUser, "SieveScript/query", `{"accountId":"`+testUser+`",`+bad+`}`)
		if name != "error" || !strings.HasPrefix(fmt.Sprint(out["type"]), "unsupported") {
			t.Errorf("%s answered %s %v, want unsupportedFilter/Sort", bad, name, out)
		}
	}
}

func TestSieveScriptValidate(t *testing.T) {
	s, _, _ := sieveServer(t, sieve.Quota{})
	for _, c := range []struct {
		script string
		want   any
	}{{keepScript, nil}, {"if true {", "invalidSieve"}} {
		out := sieveCall(t, s, "SieveScript/validate", `{"accountId":"`+testUser+`","blobId":"`+uploadAs(t, s, testUser, c.script)+`"}`)
		e, _ := out["error"].(map[string]any)
		if (c.want == nil && out["error"] != nil) || (c.want != nil && e["type"] != c.want) {
			t.Errorf("validate %q = %v, want %v", c.script, out["error"], c.want)
		}
	}
}

// validate under sieve_extensions [ihave]: require "ihave" opens nothing
// outside an ihave block, and a block under a refused extension is dead (#2297).
func TestSieveScriptValidateIhaveHoldsToSieveExtensions(t *testing.T) {
	s, _, _ := sieveServer(t, sieve.Quota{}, "ihave")
	for _, c := range []struct {
		script string
		want   any
	}{
		{`require "ihave"; execute "prog";`, "invalidSieve"},
		{`require "ihave"; fileinto "X";`, "invalidSieve"},
		{`require "ihave"; if ihave "fileinto" { fileinto "X"; }`, nil},
	} {
		out := sieveCall(t, s, "SieveScript/validate", `{"accountId":"`+testUser+`","blobId":"`+uploadAs(t, s, testUser, c.script)+`"}`)
		e, _ := out["error"].(map[string]any)
		if (c.want == nil && out["error"] != nil) || (c.want != nil && e["type"] != c.want) {
			t.Errorf("validate %q = %v, want %v", c.script, out["error"], c.want)
		}
	}
}

// GETSCRIPT over the download URL, by the blob id SieveScript/get gives.
func TestSieveScriptDownload(t *testing.T) {
	ctx := context.Background()
	s, home, st := sieveServer(t, sieve.Quota{})
	_ = st.SaveScript(ctx, testUser, home, "dl", []byte(discardScript))
	r := httptest.NewRequest(http.MethodGet, "/jmap/download/"+testUser+"/"+scriptBlobID([]byte(discardScript))+"/dl.sieve", nil)
	r.RemoteAddr = trustedPeer
	r.Header.Set(hdrUser, testUser)
	r.Header.Set(hdrSessionID, "deadbeefdeadbeef")
	r.Header.Set(hdrProxyTTL, "4")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != discardScript {
		t.Errorf("download = %d %q, want 200 %q", w.Code, w.Body.String(), discardScript)
	}
}

func TestSieveCapability(t *testing.T) {
	cases := []struct {
		name string
		q    sieve.Quota
		exts []string
		want map[string]any
	}{
		{"limits set", sieve.Quota{MaxSize: 100, MaxScripts: 3}, []string{"fileinto", "enotify"},
			map[string]any{"maxSizeScript": 100.0, "maxNumberScripts": 3.0, "maxNumberRedirects": 32.0,
				"maxSizeScriptName": 512.0, "notificationMethods": []any{"mailto"}, "externalLists": nil}},
		{"no limits, no enotify", sieve.Quota{}, []string{"fileinto"},
			map[string]any{"maxSizeScript": nil, "maxNumberScripts": nil, "notificationMethods": nil}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _, _ := sieveServer(t, c.q, c.exts...)
			raw, _ := json.Marshal(s.opts.Limits.Sieve.Account)
			var got map[string]any
			_ = json.Unmarshal(raw, &got)
			for k, v := range c.want {
				if fmt.Sprint(got[k]) != fmt.Sprint(v) {
					t.Errorf("%s = %v, want %v", k, got[k], v)
				}
			}
			if fmt.Sprint(got["sieveExtensions"]) != fmt.Sprint(c.exts) {
				t.Errorf("sieveExtensions = %v, want %v", got["sieveExtensions"], c.exts)
			}
		})
	}
}

// New advertises RFC 9661 from the options; nothing else sets the limits.
func TestNewAdvertisesSieveFromItsOptions(t *testing.T) {
	st := sieve.NewScriptStore("fs", "", nil, nil)
	with := New(Options{Sieve: &SieveScripts{Store: st, Quota: sieve.Quota{MaxScripts: 4}}})
	if with.opts.Limits.Sieve == nil || *with.opts.Limits.Sieve.Account.MaxNumberScripts != 4 {
		t.Errorf("Limits.Sieve = %+v, want the options' capability", with.opts.Limits.Sieve)
	}
	if without := New(Options{}); without.opts.Limits.Sieve != nil {
		t.Errorf("Limits.Sieve = %+v without Sieve options", without.opts.Limits.Sieve)
	}
}

// A request using the sieve capability on a server without it is refused at
// the door, not answered method by method.
func TestSieveCapabilityIsRefusedWhereItIsNotServed(t *testing.T) {
	s, _, _ := storedServerWithMessageAt(t, "Subject: seed\r\n\r\nseed\r\n", 0)
	body := `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:sieve"],"methodCalls":[["SieveScript/get",{"accountId":"` + testUser + `"},"c0"]]}`
	w := postAPIRaw(t, s, body)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "unknownCapability") {
		t.Errorf("status %d %s, want 400 unknownCapability", w.Code, w.Body)
	}
}

// A stored script's blob id is a valid source: the bytes are copied.
func TestSieveScriptCreateFromAScriptBlob(t *testing.T) {
	ctx := context.Background()
	s, home, st := sieveServer(t, sieve.Quota{})
	_ = st.SaveScript(ctx, testUser, home, "orig", []byte(discardScript))
	out := sieveCall(t, s, "SieveScript/set", createArgs(scriptBlobID([]byte(discardScript)), "copy", ""))
	if entry(out, "created", "c1")["id"] == nil {
		t.Fatalf("create from a script blob = %v", out)
	}
	if src, _, _ := st.GetScript(ctx, testUser, home, "copy"); string(src) != discardScript {
		t.Errorf("copy holds %q, want %q", src, discardScript)
	}
}

// The name check before the store is not the guard: a name taken after the
// view was read (another session) is refused by the store, and nothing is lost.
func TestSieveScriptStaleViewLosesNothing(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		run  func(s *Server, h *userHandle, v *sieveView, blob string) *jmapcore.SetError
	}{
		{"create", func(s *Server, h *userHandle, v *sieveView, blob string) *jmapcore.SetError {
			_, serr := s.sieveCreate(ctx, h, testUser, v, json.RawMessage(`{"name":"taken","blobId":"`+blob+`"}`))
			return serr
		}},
		{"rename with new content", func(s *Server, h *userHandle, v *sieveView, blob string) *jmapcore.SetError {
			_, serr := s.sieveUpdate(ctx, h, testUser, v, v.ix.IDs["mine"], json.RawMessage(`{"name":"taken","blobId":"`+blob+`"}`))
			return serr
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, home, st := sieveServer(t, sieve.Quota{})
			_ = st.SaveScript(ctx, testUser, home, "mine", []byte(keepScript))
			blob := uploadAs(t, s, testUser, "stop;\n")
			h, err := s.opts.Storage.open(testUser, "")
			if err != nil {
				t.Fatal(err)
			}
			defer h.close()
			v, err := s.sieveRead(ctx, h, testUser)
			if err != nil {
				t.Fatal(err)
			}
			_ = st.SaveScript(ctx, testUser, home, "taken", []byte(discardScript))
			serr := c.run(s, h, &v, blob)
			if serr == nil || serr.Type != jmapcore.SetErrAlreadyExists || serr.ExistingID == "" {
				t.Fatalf("SetError = %+v, want alreadyExists with existingId", serr)
			}
			for name, want := range map[string]string{"taken": discardScript, "mine": keepScript} {
				if src, _, _ := st.GetScript(ctx, testUser, home, name); string(src) != want {
					t.Errorf("%s holds %q, want %q", name, src, want)
				}
			}
		})
	}
}

func TestExistingIDFallsBackToTheCallsStart(t *testing.T) {
	start := sieve.ScriptIndex{IDs: map[string]string{"x": "S1"}}
	cases := []struct {
		name  string
		fresh sieve.ScriptIndex
		err   error
		start sieve.ScriptIndex
		want  string
	}{
		{"the fresh read names it", sieve.ScriptIndex{IDs: map[string]string{"x": "S9"}}, nil, start, "S9"},
		{"the fresh read failed", sieve.ScriptIndex{}, errors.New("lock service down"), start, "S1"},
		{"the fresh read does not have it", sieve.ScriptIndex{IDs: map[string]string{}}, nil, start, "S1"},
		{"neither has it", sieve.ScriptIndex{}, errors.New("down"), sieve.ScriptIndex{IDs: map[string]string{}}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := existingID(c.fresh, c.err, c.start, testUser, "x"); got != c.want {
				t.Errorf("existingID = %q, want %q", got, c.want)
			}
		})
	}
}
