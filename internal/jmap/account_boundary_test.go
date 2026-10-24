package jmap

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/jmapcore"

	"github.com/yarilomail/yarilo/internal/sieve"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

const foreignUser = "u2@example.com"

// twoAccountServer serves testUser and foreignUser from separate homes, one
// message each, and returns the foreign message's id.
func twoAccountServer(t *testing.T) (*Server, string) {
	t.Helper()
	locker := &testLocker{}
	infos := map[string]*mailbox.UserInfo{}
	var foreignGUID [16]byte
	for _, user := range []string{testUser, foreignUser} {
		info := &mailbox.UserInfo{Username: user, Home: t.TempDir(), Separator: "/"}
		infos[user] = info
		guid := seedMessage(t, info, locker, "Subject: "+user+"\r\n\r\nbody\r\n")
		if user == foreignUser {
			foreignGUID = guid
		}
	}
	s := New(Options{
		Trust:  ResolveTrust(false, true, []*net.IPNet{mustCIDR(t, "192.0.2.0/24")}),
		Limits: testLimits(),
		Storage: &Storage{
			Mailbox: maildir.New(),
			Index:   file.New(file.WithLocker(locker)),
			ResolveUser: func(user string) (*mailbox.UserInfo, error) {
				if info, ok := infos[user]; ok {
					return info, nil
				}
				return nil, fmt.Errorf("no user %s", user)
			},
			Locker: locker,
		},
		Sieve:      &SieveScripts{Store: sieve.NewScriptStore("fs", "", nil, nil), Vacation: true},
		Submission: &Submission{Sender: boundaryRelay, TTL: time.Hour},
	})
	if err := s.opts.Sieve.Store.SaveScript(context.Background(), foreignUser, infos[foreignUser].Home, "theirs", []byte("keep;\n")); err != nil {
		t.Fatal(err)
	}
	return s, hex.EncodeToString(foreignGUID[:])
}

// callAs runs one method as user and returns the response name and arguments.
func callAs(t *testing.T, s *Server, user, method, args string) (string, map[string]any) {
	t.Helper()
	using, _ := json.Marshal(declaredCapabilities(s.opts.Limits))
	body := fmt.Sprintf(`{"using":%s,"methodCalls":[[%q,%s,"c0"]]}`, using, method, args)
	r := apiRequest(body)
	r.Header.Set(hdrUser, user)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("%s: status %d: %s", method, w.Code, w.Body)
	}
	var resp struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || len(resp.MethodResponses) != 1 {
		t.Fatalf("%s: bad response %v: %s", method, err, w.Body)
	}
	var name string
	out := map[string]any{}
	_ = json.Unmarshal(resp.MethodResponses[0][0], &name)
	_ = json.Unmarshal(resp.MethodResponses[0][1], &out)
	return name, out
}

func methodNames(s *Server) []string {
	var names []string
	for name := range s.registry(&lazyStore{storage: s.opts.Storage, user: testUser}, testUser) {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func wantMethodError(t *testing.T, name string, out map[string]any, errType string) {
	t.Helper()
	if name != "error" || out["type"] != errType {
		t.Errorf("answered %s %v, want error %s", name, out, errType)
	}
}

// Every method that names an account refuses one that is not the caller's;
// derived from the registry, so a new method is covered on registration.
func TestEveryMethodRefusesAForeignAccount(t *testing.T) {
	s, _ := twoAccountServer(t)
	for _, method := range methodNames(s) {
		if method == "Core/echo" {
			continue
		}
		t.Run(method, func(t *testing.T) {
			name, out := callAs(t, s, testUser, method, `{"accountId":"`+jmapcore.AccountID(foreignUser)+`"}`)
			wantMethodError(t, name, out, "accountNotFound")
		})
	}
}

// Every method that names an account refuses a call that names none (#2210).
func TestEveryMethodRequiresAnAccount(t *testing.T) {
	s, _ := twoAccountServer(t)
	for _, method := range methodNames(s) {
		if method == "Core/echo" {
			continue
		}
		t.Run(method, func(t *testing.T) {
			// The description, not the type: a missing sinceState or ids is
			// invalidArguments too, and would pass for the wrong reason.
			name, out := callAs(t, s, testUser, method, `{}`)
			if name != "error" || out["type"] != "invalidArguments" || out["description"] != "accountId is required" {
				t.Errorf("answered %s %v, want invalidArguments naming accountId", name, out)
			}
		})
	}
}

const (
	kindForeignAccount = "foreign account"
	kindEmptyAccount   = "empty account"
	kindForeignObject  = "foreign object"
)

// boundaryRows are the account-boundary rows of every mutating method. FOREIGN
// stands for the other account's message id.
var boundaryRows = []struct {
	method, kind, args string
	check              func(t *testing.T, s *Server, foreign, name string, out map[string]any)
}{
	{"Email/set", kindForeignAccount, `{"accountId":"` + jmapcore.AccountID(foreignUser) + `","update":{"FOREIGN":{"keywords/$flagged":true}}}`,
		func(t *testing.T, _ *Server, _, name string, out map[string]any) {
			wantMethodError(t, name, out, "accountNotFound")
		}},
	{"Email/set", kindEmptyAccount, `{"update":{"FOREIGN":{"keywords/$flagged":true}}}`,
		func(t *testing.T, _ *Server, _, name string, out map[string]any) {
			wantMethodError(t, name, out, "invalidArguments")
		}},
	{"Email/set", kindForeignObject, `{"accountId":"` + testAccount + `","update":{"FOREIGN":{"keywords/$flagged":true}}}`,
		func(t *testing.T, s *Server, foreign, name string, out map[string]any) {
			notUpdated, _ := out["notUpdated"].(map[string]any)
			setErr, _ := notUpdated[foreign].(map[string]any)
			if name != "Email/set" || setErr["type"] != "notFound" {
				t.Errorf("answered %s %v, want notUpdated %s notFound", name, out, foreign)
			}
			_, got := callAs(t, s, foreignUser, "Email/get",
				`{"accountId":"`+jmapcore.AccountID(foreignUser)+`","ids":["`+foreign+`"],"properties":["keywords"]}`)
			if list, _ := got["list"].([]any); len(list) != 1 || strings.Contains(fmt.Sprint(list[0]), "$flagged") {
				t.Errorf("the foreign message changed: %v", got)
			}
		}},
	{"Email/import", kindForeignAccount, `{"accountId":"` + jmapcore.AccountID(foreignUser) + `","emails":{"c1":{"blobId":"FOREIGN","mailboxIds":{"INBOXID":true}}}}`,
		func(t *testing.T, _ *Server, _, name string, out map[string]any) {
			wantMethodError(t, name, out, "accountNotFound")
		}},
	{"Email/import", kindEmptyAccount, `{"emails":{"c1":{"blobId":"FOREIGN","mailboxIds":{"INBOXID":true}}}}`,
		func(t *testing.T, _ *Server, _, name string, out map[string]any) {
			wantMethodError(t, name, out, "invalidArguments")
		}},
	{"Email/import", kindForeignObject, `{"accountId":"` + testAccount + `","emails":{"c1":{"blobId":"FOREIGN","mailboxIds":{"INBOXID":true}}}}`,
		func(t *testing.T, _ *Server, _, name string, out map[string]any) {
			notCreated, _ := out["notCreated"].(map[string]any)
			setErr, _ := notCreated["c1"].(map[string]any)
			if name != "Email/import" || setErr["type"] != "invalidProperties" || !strings.Contains(fmt.Sprint(setErr["properties"]), "blobId") {
				t.Errorf("answered %s %v, want notCreated c1 invalidProperties blobId", name, out)
			}
		}},
	{"Mailbox/set", kindForeignAccount, `{"accountId":"` + jmapcore.AccountID(foreignUser) + `","destroy":["FOREIGNBOX"]}`,
		func(t *testing.T, _ *Server, _, name string, out map[string]any) {
			wantMethodError(t, name, out, "accountNotFound")
		}},
	{"Mailbox/set", kindEmptyAccount, `{"destroy":["FOREIGNBOX"]}`,
		func(t *testing.T, _ *Server, _, name string, out map[string]any) {
			wantMethodError(t, name, out, "invalidArguments")
		}},
	{"SieveScript/set", kindForeignAccount, `{"accountId":"` + jmapcore.AccountID(foreignUser) + `","destroy":["S1"]}`,
		func(t *testing.T, _ *Server, _, name string, out map[string]any) {
			wantMethodError(t, name, out, "accountNotFound")
		}},
	{"SieveScript/set", kindEmptyAccount, `{"destroy":["S1"]}`,
		func(t *testing.T, _ *Server, _, name string, out map[string]any) {
			wantMethodError(t, name, out, "invalidArguments")
		}},
	// S1 is the other account's script; ids are per account.
	{"SieveScript/set", kindForeignObject, `{"accountId":"` + testAccount + `","destroy":["S1"]}`,
		func(t *testing.T, s *Server, _, name string, out map[string]any) {
			notDestroyed, _ := out["notDestroyed"].(map[string]any)
			setErr, _ := notDestroyed["S1"].(map[string]any)
			if name != "SieveScript/set" || setErr["type"] != "notFound" {
				t.Errorf("answered %s %v, want notDestroyed S1 notFound", name, out)
			}
			_, got := callAs(t, s, foreignUser, "SieveScript/get", `{"accountId":"`+jmapcore.AccountID(foreignUser)+`","ids":["S1"]}`)
			if list, _ := got["list"].([]any); len(list) != 1 {
				t.Errorf("the other account's script is gone: %v", got)
			}
		}},
	{"EmailSubmission/set", kindForeignAccount, `{"accountId":"` + jmapcore.AccountID(foreignUser) + `","create":{"c1":{"identityId":"x","emailId":"FOREIGN"}}}`,
		func(t *testing.T, _ *Server, _, name string, out map[string]any) {
			wantMethodError(t, name, out, "accountNotFound")
		}},
	{"EmailSubmission/set", kindEmptyAccount, `{"create":{"c1":{"identityId":"x","emailId":"FOREIGN"}}}`,
		func(t *testing.T, _ *Server, _, name string, out map[string]any) {
			wantMethodError(t, name, out, "invalidArguments")
		}},
	// FOREIGN is the other account's message: not found here, never sent.
	{"EmailSubmission/set", kindForeignObject, `{"accountId":"` + testAccount + `","create":{"c1":{"identityId":"IDENTITY","emailId":"FOREIGN"}}}`,
		func(t *testing.T, _ *Server, _, name string, out map[string]any) {
			e := entry(out, "notCreated", "c1")
			if name != "EmailSubmission/set" || e["type"] != "invalidProperties" || !strings.Contains(fmt.Sprint(e["properties"]), "emailId") {
				t.Errorf("answered %s %v, want notCreated c1 invalidProperties emailId", name, out)
			}
			if n := len(boundaryRelay.all()); n != 0 {
				t.Errorf("the other account's message reached the relay (%d sends)", n)
			}
		}},
	{"VacationResponse/set", kindForeignAccount, `{"accountId":"` + jmapcore.AccountID(foreignUser) + `","update":{"singleton":{"isEnabled":true}}}`,
		func(t *testing.T, _ *Server, _, name string, out map[string]any) {
			wantMethodError(t, name, out, "accountNotFound")
		}},
	{"VacationResponse/set", kindEmptyAccount, `{"update":{"singleton":{"isEnabled":true}}}`,
		func(t *testing.T, _ *Server, _, name string, out map[string]any) {
			wantMethodError(t, name, out, "invalidArguments")
		}},
	// "singleton" is each account's own: enabling ours leaves theirs as it was.
	{"VacationResponse/set", kindForeignObject, `{"accountId":"` + testAccount + `","update":{"singleton":{"isEnabled":true}}}`,
		func(t *testing.T, s *Server, _, name string, out map[string]any) {
			if name != "VacationResponse/set" || entry(out, "notUpdated", "singleton") != nil {
				t.Errorf("answered %s %v, want our singleton updated", name, out)
			}
			_, got := callAs(t, s, foreignUser, "SieveScript/get", `{"accountId":"`+jmapcore.AccountID(foreignUser)+`","ids":null,"properties":["name"]}`)
			if fmt.Sprint(got["list"]) != "[map[id:S1 name:theirs]]" {
				t.Errorf("the other account's scripts changed: %v", got["list"])
			}
		}},
	{"Identity/set", kindForeignAccount, `{"accountId":"` + jmapcore.AccountID(foreignUser) + `","destroy":["FOREIGNIDENTITY"]}`,
		func(t *testing.T, _ *Server, _, name string, out map[string]any) {
			wantMethodError(t, name, out, "accountNotFound")
		}},
	{"Identity/set", kindEmptyAccount, `{"destroy":["FOREIGNIDENTITY"]}`,
		func(t *testing.T, _ *Server, _, name string, out map[string]any) {
			wantMethodError(t, name, out, "invalidArguments")
		}},
	{"Identity/set", kindForeignObject, `{"accountId":"` + testAccount + `","destroy":["FOREIGNIDENTITY"]}`,
		func(t *testing.T, _ *Server, _, name string, out map[string]any) {
			if name != "Identity/set" || entry(out, "notDestroyed", identityID(foreignUser))["type"] != "forbidden" {
				t.Errorf("answered %s %v, want notDestroyed forbidden", name, out)
			}
		}},
	// Resolved globally, the other INBOX would answer forbidden, not notFound.
	{"Mailbox/set", kindForeignObject, `{"accountId":"` + testAccount + `","destroy":["FOREIGNBOX"]}`,
		func(t *testing.T, _ *Server, _, name string, out map[string]any) {
			notDestroyed, _ := out["notDestroyed"].(map[string]any)
			setErr, _ := notDestroyed[foreignBoxID].(map[string]any)
			if name != "Mailbox/set" || setErr["type"] != "notFound" {
				t.Errorf("answered %s %v, want notDestroyed notFound", name, out)
			}
		}},
}

// boundaryRelay is the relay of the two-account server; a row that refuses
// must leave it untouched.
var boundaryRelay = &fakeRelay{}

// foreignBoxID is set per row run to the other account's INBOX id.
var foreignBoxID string

// inboxIDOf is user's INBOX mailbox id.
func inboxIDOf(t *testing.T, s *Server, user string) string {
	t.Helper()
	_, out := callAs(t, s, user, "Mailbox/get", `{"accountId":"`+jmapcore.AccountID(user)+`"}`)
	for _, mb := range out["list"].([]any) {
		if m := mb.(map[string]any); m["name"] == "INBOX" {
			return m["id"].(string)
		}
	}
	t.Fatalf("no INBOX for %s", user)
	return ""
}

// inboxID is the caller's INBOX mailbox id.
func inboxID(t *testing.T, s *Server) string {
	t.Helper()
	_, out := callAs(t, s, testUser, "Mailbox/get", `{"accountId":"`+testAccount+`"}`)
	for _, mb := range out["list"].([]any) {
		m := mb.(map[string]any)
		if m["name"] == "INBOX" || m["role"] == "inbox" {
			return m["id"].(string)
		}
	}
	t.Fatal("no INBOX")
	return ""
}

func TestMutatingMethodsKeepTheAccountBoundary(t *testing.T) {
	for _, row := range boundaryRows {
		t.Run(row.method+"/"+row.kind, func(t *testing.T) {
			s, foreign := twoAccountServer(t)
			foreignBoxID = inboxIDOf(t, s, foreignUser)
			args := strings.NewReplacer("FOREIGNBOX", foreignBoxID, "FOREIGNIDENTITY", identityID(foreignUser),
				"IDENTITY", identityID(testUser), "FOREIGN", foreign, "INBOXID", inboxID(t, s)).Replace(row.args)
			name, out := callAs(t, s, testUser, row.method, args)
			row.check(t, s, foreign, name, out)
		})
	}
}

// A mutating method registered without all three boundary rows fails here.
func TestEveryMutatingMethodHasBoundaryRows(t *testing.T) {
	s, _ := twoAccountServer(t)
	have := map[string]map[string]bool{}
	for _, row := range boundaryRows {
		if have[row.method] == nil {
			have[row.method] = map[string]bool{}
		}
		have[row.method][row.kind] = true
	}
	for _, method := range methodNames(s) {
		if !strings.HasSuffix(method, "/set") && !strings.HasSuffix(method, "/import") && !strings.HasSuffix(method, "/copy") {
			continue
		}
		for _, kind := range []string{kindForeignAccount, kindEmptyAccount, kindForeignObject} {
			if !have[method][kind] {
				t.Errorf("%s has no %q boundary row", method, kind)
			}
		}
	}
}
