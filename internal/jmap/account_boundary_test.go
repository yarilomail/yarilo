package jmap

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

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
	})
	return s, hex.EncodeToString(foreignGUID[:])
}

// callAs runs one method as user and returns the response name and arguments.
func callAs(t *testing.T, s *Server, user, method, args string) (string, map[string]any) {
	t.Helper()
	using, _ := json.Marshal(declaredCapabilities())
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
			name, out := callAs(t, s, testUser, method, `{"accountId":"`+foreignUser+`"}`)
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
	{"Email/set", kindForeignAccount, `{"accountId":"` + foreignUser + `","update":{"FOREIGN":{"keywords/$flagged":true}}}`,
		func(t *testing.T, _ *Server, _, name string, out map[string]any) {
			wantMethodError(t, name, out, "accountNotFound")
		}},
	{"Email/set", kindEmptyAccount, `{"update":{"FOREIGN":{"keywords/$flagged":true}}}`,
		func(t *testing.T, _ *Server, _, name string, out map[string]any) {
			wantMethodError(t, name, out, "invalidArguments")
		}},
	{"Email/set", kindForeignObject, `{"accountId":"` + testUser + `","update":{"FOREIGN":{"keywords/$flagged":true}}}`,
		func(t *testing.T, s *Server, foreign, name string, out map[string]any) {
			notUpdated, _ := out["notUpdated"].(map[string]any)
			setErr, _ := notUpdated[foreign].(map[string]any)
			if name != "Email/set" || setErr["type"] != "notFound" {
				t.Errorf("answered %s %v, want notUpdated %s notFound", name, out, foreign)
			}
			_, got := callAs(t, s, foreignUser, "Email/get",
				`{"accountId":"`+foreignUser+`","ids":["`+foreign+`"],"properties":["keywords"]}`)
			if list, _ := got["list"].([]any); len(list) != 1 || strings.Contains(fmt.Sprint(list[0]), "$flagged") {
				t.Errorf("the foreign message changed: %v", got)
			}
		}},
}

func TestMutatingMethodsKeepTheAccountBoundary(t *testing.T) {
	for _, row := range boundaryRows {
		t.Run(row.method+"/"+row.kind, func(t *testing.T) {
			s, foreign := twoAccountServer(t)
			name, out := callAs(t, s, testUser, row.method, strings.ReplaceAll(row.args, "FOREIGN", foreign))
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
