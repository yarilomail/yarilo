package jmap

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/sieve"
	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/jmapcore"
)

func vacationServer(t *testing.T, exts ...string) (*Server, string, *sieve.IndexedStore) {
	t.Helper()
	s, home, st := sieveServer(t, sieve.Quota{}, exts...)
	s.opts.Sieve.Vacation = true
	s.opts.Limits.Vacation = &jmapcore.VacationCapability{}
	return s, home, st
}

func vacationGetOne(t *testing.T, s *Server) map[string]any {
	t.Helper()
	out := sieveCall(t, s, "VacationResponse/get", `{"accountId":"`+testAccount+`","ids":null}`)
	list, _ := out["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("VacationResponse/get list = %v", out)
	}
	return list[0].(map[string]any)
}

func vacationUpdate(t *testing.T, s *Server, patch string) map[string]any {
	t.Helper()
	return sieveCall(t, s, "VacationResponse/set", `{"accountId":"`+testAccount+`","update":{"singleton":`+patch+`}}`)
}

func saveActive(t *testing.T, st *sieve.IndexedStore, home, name, src string) {
	t.Helper()
	ctx := context.Background()
	if err := st.SaveScript(ctx, testUser, home, name, []byte(src)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetActive(ctx, testUser, home, name); err != nil {
		t.Fatal(err)
	}
}

func activeName(t *testing.T, st *sieve.IndexedStore, home string) string {
	t.Helper()
	name, err := st.ActiveScriptName(context.Background(), testUser, home)
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func TestVacationGetWithoutAScript(t *testing.T) {
	s, _, _ := vacationServer(t)
	got := vacationGetOne(t, s)
	if got["id"] != "singleton" || got["isEnabled"] != false || got["textBody"] != nil || got["htmlBody"] != nil || got["subject"] != nil {
		t.Errorf("default = %v", got)
	}
	out := sieveCall(t, s, "VacationResponse/get", `{"accountId":"`+testAccount+`","ids":["singleton","other"]}`)
	if fmt.Sprint(out["notFound"]) != "[other]" {
		t.Errorf("notFound = %v, want [other]", out["notFound"])
	}
	name, out := callAs(t, s, testUser, "VacationResponse/get", `{"accountId":"`+testAccount+`","properties":["nosuch"]}`)
	if name != "error" || out["type"] != "invalidArguments" {
		t.Errorf("unknown property answered %s %v", name, out)
	}
}

// Enabling with no active script activates the managed one (RFC 8621 §8).
func TestVacationEnableActivates(t *testing.T) {
	s, home, st := vacationServer(t)
	out := vacationUpdate(t, s, `{"isEnabled":true,"subject":"Away","textBody":"Back on Monday."}`)
	if _, ok := out["updated"].(map[string]any)["singleton"]; !ok {
		t.Fatalf("set = %v", out)
	}
	if out["oldState"] == out["newState"] {
		t.Errorf("state did not move: %v", out)
	}
	if got := activeName(t, st, home); got != vacationScript {
		t.Errorf("active script %q, want the managed one", got)
	}
	got := vacationGetOne(t, s)
	if got["isEnabled"] != true || got["subject"] != "Away" || got["textBody"] != "Back on Monday." {
		t.Errorf("after enabling = %v", got)
	}
}

// The user's own active script is never displaced: enabling needs it to
// include the managed script at its top level, and is refused otherwise.
func TestVacationEnableAgainstTheUsersScript(t *testing.T) {
	cases := []struct {
		name    string
		own     string
		allowed bool
	}{
		{"no include: forbidden", `keep;`, false},
		{"top-level include", `require "include"; include :personal "` + vacationScript + `";`, true},
		{"include without a location is personal", `require "include"; include "` + vacationScript + `";`, true},
		{"include with :once :optional", `require "include"; include :once :optional "` + vacationScript + `";`, true},
		{"include inside a test does not count", `require "include"; if true { include "` + vacationScript + `"; }`, false},
		{"a :global include does not count", `require "include"; include :global "` + vacationScript + `";`, false},
		{"another script included", `require "include"; include "other";`, false},
		{"a command naming it that is not include", `require "fileinto"; fileinto "` + vacationScript + `";`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, home, st := vacationServer(t)
			saveActive(t, st, home, "mine", c.own)
			out := vacationUpdate(t, s, `{"isEnabled":true}`)
			e := entry(out, "notUpdated", "singleton")
			if c.allowed != (e == nil) {
				t.Fatalf("set = %v, want allowed %v", out, c.allowed)
			}
			if !c.allowed && (e["type"] != "forbidden" || !strings.Contains(fmt.Sprint(e["description"]), "include")) {
				t.Errorf("refusal = %v, want forbidden naming include", e)
			}
			if got := activeName(t, st, home); got != "mine" {
				t.Errorf("active script %q, want the user's own", got)
			}
			if got := vacationGetOne(t, s); got["isEnabled"] != c.allowed {
				t.Errorf("isEnabled = %v, want %v", got["isEnabled"], c.allowed)
			}
		})
	}
}

// A stored enabled response that no longer runs reads as disabled.
func TestVacationEnabledButNotRunning(t *testing.T) {
	s, home, st := vacationServer(t)
	vacationUpdate(t, s, `{"isEnabled":true}`)
	saveActive(t, st, home, "mine", `keep;`)
	if got := vacationGetOne(t, s); got["isEnabled"] != false {
		t.Errorf("isEnabled = %v with another script active", got["isEnabled"])
	}
}

// A managed script edited out of shape reads as no response; enabling writes it anew.
func TestVacationBrokenHeader(t *testing.T) {
	s, home, st := vacationServer(t)
	vacationUpdate(t, s, `{"isEnabled":true,"subject":"Away"}`)
	if err := st.SaveScript(context.Background(), testUser, home, vacationScript, []byte(`require "vacation"; vacation "edited";`)); err != nil {
		t.Fatal(err)
	}
	if got := vacationGetOne(t, s); got["isEnabled"] != false || got["subject"] != nil {
		t.Errorf("after a hand edit = %v, want the default", got)
	}
	vacationUpdate(t, s, `{"isEnabled":true}`)
	src, _, _ := st.GetScript(context.Background(), testUser, home, vacationScript)
	if !strings.HasPrefix(string(src), vacationHeader) {
		t.Errorf("the script was not written anew:\n%s", src)
	}
	if got := vacationGetOne(t, s); got["isEnabled"] != true {
		t.Errorf("after enabling = %v", got)
	}
}

func TestVacationSetRefusals(t *testing.T) {
	s, _, _ := vacationServer(t)
	cases := []struct {
		patch string
		prop  string
	}{
		{`{"fromDate":"2026-10-18"}`, "fromDate"},
		{`{"toDate":"2026-10-18T10:00:00+02:00"}`, "toDate"},
		{`{"fromDate":"2026-13-40T10:00:00Z"}`, "fromDate"},
		{`{"subject":"a\r\nBcc: x@ext.test"}`, "subject"},
		{`{"subject":"a\tb"}`, "subject"},
		{`{"subject":"a\u007fb"}`, "subject"},
		{`{"isEnabled":null}`, "isEnabled"},
		{`{"textBody":7}`, "textBody"},
		{`{"id":"other"}`, "id"},
		{`{"nosuch":1}`, "nosuch"},
	}
	for _, c := range cases {
		out := vacationUpdate(t, s, c.patch)
		if e := entry(out, "notUpdated", "singleton"); e["type"] != "invalidProperties" || fmt.Sprint(e["properties"]) != "["+c.prop+"]" {
			t.Errorf("%s: %v, want invalidProperties [%s]", c.patch, e, c.prop)
		}
	}
	out := sieveCall(t, s, "VacationResponse/set", `{"accountId":"`+testAccount+`","create":{"c":{}},"destroy":["singleton","x"],"update":{"x":{}}}`)
	checks := []struct{ bucket, key, want string }{
		{"notCreated", "c", "singleton"}, {"notDestroyed", "singleton", "singleton"},
		{"notDestroyed", "x", "notFound"}, {"notUpdated", "x", "notFound"},
	}
	for _, c := range checks {
		if e := entry(out, c.bucket, c.key); e["type"] != c.want {
			t.Errorf("%s %s = %v, want %s", c.bucket, c.key, e, c.want)
		}
	}
	name, out := callAs(t, s, testUser, "VacationResponse/set", `{"accountId":"`+testAccount+`","ifInState":"nope","update":{"singleton":{"isEnabled":false}}}`)
	if name != "error" || out["type"] != "stateMismatch" {
		t.Errorf("ifInState answered %s %v", name, out)
	}
}

// What a client writes comes back as written, through the script's header.
func TestVacationRoundTrip(t *testing.T) {
	s, _, _ := vacationServer(t)
	patch := map[string]any{
		"isEnabled": false, "fromDate": "2026-10-18T08:00:00Z", "toDate": "2026-10-25T08:00:00.5Z",
		"subject":  `Away */ "quoted" \ back`,
		"textBody": ".a dotted line\nand */ more\n.", "htmlBody": "<p>Away</p>",
	}
	raw, _ := json.Marshal(patch)
	if out := vacationUpdate(t, s, string(raw)); entry(out, "notUpdated", "singleton") != nil {
		t.Fatalf("set = %v", out)
	}
	got := vacationGetOne(t, s)
	for k, want := range patch {
		if got[k] != want {
			t.Errorf("%s = %#v, want %#v", k, got[k], want)
		}
	}
}

// The script the object becomes is what delivery runs: the dates bound the
// reply, :mime carries the HTML, and a disabled response sends nothing.
func TestVacationScriptRunsAtDelivery(t *testing.T) {
	now := time.Now().UTC()
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	cases := []struct {
		name  string
		first string // a patch applied before
		patch string
		reply bool
		check func(t *testing.T, body string, mime bool, subject string)
	}{
		{"enabled, no dates", "", `{"isEnabled":true,"subject":"Away","textBody":"Back on Monday."}`, true,
			func(t *testing.T, body string, mime bool, subject string) {
				if subject != "Away" || mime || !strings.Contains(body, "Back on Monday.") {
					t.Errorf("reply %q mime %v subject %q", body, mime, subject)
				}
			}},
		{"disabled", "", `{"isEnabled":false}`, false, nil},
		{"disabled while still active", `{"isEnabled":true}`, `{"isEnabled":false}`, false, nil},
		{"no bodies: the default message", "", `{"isEnabled":true}`, true,
			func(t *testing.T, body string, mime bool, _ string) {
				if mime || !strings.Contains(body, vacationDefaultText) {
					t.Errorf("reply %q mime %v", body, mime)
				}
			}},
		{"inside the dates", "", `{"isEnabled":true,"fromDate":"` + at(-time.Hour) + `","toDate":"` + at(time.Hour) + `"}`, true, nil},
		{"before fromDate", "", `{"isEnabled":true,"fromDate":"` + at(time.Hour) + `"}`, false, nil},
		{"at toDate or after", "", `{"isEnabled":true,"toDate":"` + at(-time.Hour) + `"}`, false, nil},
		{"html as :mime with a text part", "", `{"isEnabled":true,"htmlBody":"<p>Away <b>now</b></p>"}`, true,
			func(t *testing.T, body string, mime bool, _ string) {
				if !mime || !strings.Contains(body, "multipart/alternative") || !strings.Contains(body, "Away now") || !strings.Contains(body, "<p>Away <b>now</b></p>") {
					t.Errorf("mime %v body:\n%s", mime, body)
				}
			}},
		{"a dotted line survives", "", `{"isEnabled":true,"textBody":".x\n..y"}`, true,
			func(t *testing.T, body string, _ bool, _ string) {
				if !strings.Contains(body, ".x\r\n..y") {
					t.Errorf("body %q", body)
				}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, home, st := vacationServer(t)
			if c.first != "" {
				vacationUpdate(t, s, c.first)
			}
			if out := vacationUpdate(t, s, c.patch); entry(out, "notUpdated", "singleton") != nil {
				t.Fatalf("set = %v", out)
			}
			e := sieve.New(config.SieveConfig{Enabled: true, MaxRedirects: 32, MaxScriptSize: 65536, DefaultName: "yarilo"}, nil, nil, nil)
			res, err := e.Filter(context.Background(), sieve.FilterOptions{Username: testUser, HomeDir: home,
				EnvFrom: "bob@ext.test", EnvTo: testUser, MsgRaw: []byte("From: bob@ext.test\r\nTo: " + testUser + "\r\nSubject: hi\r\n\r\nb\r\n")})
			if err != nil {
				t.Fatal(err)
			}
			if c.first != "" && activeName(t, st, home) != vacationScript {
				t.Fatal("the managed script is not active, so the row tests nothing")
			}
			if got := res != nil && len(res.VacationReplies) == 1; got != c.reply {
				t.Fatalf("reply = %v, want %v (%+v)", got, c.reply, res)
			}
			if c.check != nil {
				r := res.VacationReplies[0]
				c.check(t, r.Body, r.IsMime, r.Subject)
			}
		})
	}
}

// The script it writes is held to sieve_extensions like any other.
func TestVacationDatesNeedTheirExtensions(t *testing.T) {
	s, _, _ := vacationServer(t, "vacation")
	if out := vacationUpdate(t, s, `{"isEnabled":true}`); entry(out, "notUpdated", "singleton") != nil {
		t.Fatalf("plain enable = %v", out)
	}
	out := vacationUpdate(t, s, `{"fromDate":"2026-10-18T08:00:00Z"}`)
	if e := entry(out, "notUpdated", "singleton"); e["type"] != "invalidSieve" {
		t.Errorf("dates without date and relational allowed = %v", e)
	}
}

// New advertises and serves VacationResponse only when the scripts allow it.
func TestNewAdvertisesVacation(t *testing.T) {
	for _, on := range []bool{true, false} {
		s := New(Options{Limits: testLimits(), Storage: &Storage{}, Sieve: &SieveScripts{Store: sieve.NewScriptStore("fs", "", nil, nil), Vacation: on}})
		if got := s.opts.Limits.Vacation != nil; got != on {
			t.Errorf("Vacation %v: advertised %v", on, got)
		}
		_, has := s.registry(&lazyStore{storage: s.opts.Storage, user: testUser}, testUser)["VacationResponse/get"]
		if has != on {
			t.Errorf("Vacation %v: registered %v", on, has)
		}
	}
}

// The dates compare as strings with currentdate's "iso8601" under :zone "+0000",
// which writes the offset as +00:00, so a UTCDate's Z is written the same way.
func TestVacationDateHasCurrentdatesForm(t *testing.T) {
	for in, want := range map[string]string{
		"2026-10-18T08:00:00Z":   "2026-10-18T08:00:00+00:00",
		"2026-10-18T08:00:00.5Z": "2026-10-18T08:00:00+00:00",
	} {
		if got := sieveISO8601(in); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}
