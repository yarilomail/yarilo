package jmap

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/userstate/counters"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

const aliasAddr = "a@example.com"

func identityServer(t *testing.T) *Server {
	t.Helper()
	s, _, _ := submissionServer(t, draftHeaders+"\r\nbody\r\n", &fakeRelay{}, Submission{}, aliasAddr)
	return s
}

func identitySet(t *testing.T, s *Server, args string) map[string]any {
	t.Helper()
	return subCall(t, s, "Identity/set", `{"accountId":"`+testAccount+`",`+args+`}`)
}

func identityList(t *testing.T, s *Server) (map[string]map[string]any, string) {
	t.Helper()
	got := subCall(t, s, "Identity/get", `{"accountId":"`+testAccount+`","ids":null}`)
	out := map[string]map[string]any{}
	for _, v := range got["list"].([]any) {
		m := v.(map[string]any)
		out[m["id"].(string)] = m
	}
	return out, got["state"].(string)
}

// A created identity reads back with every property it was given, may be
// destroyed, and takes the address in the account's own case.
func TestIdentitySetCreatesAndReadsBack(t *testing.T) {
	s := identityServer(t)
	_, before := identityList(t, s)
	out := identitySet(t, s, `"create":{"n":{"email":"A@Example.com","name":"Work","replyTo":[{"name":null,"email":"r@example.com"}],"textSignature":"--\nA","htmlSignature":"<p>A</p>"}}`)
	c := entry(out, "created", "n")
	id, _ := c["id"].(string)
	if id == "" || c["mayDelete"] != true || out["oldState"] != before || out["newState"] == before {
		t.Fatalf("Identity/set = %v", out)
	}
	list, after := identityList(t, s)
	got := list[id]
	if got == nil || got["email"] != aliasAddr || got["name"] != "Work" || got["textSignature"] != "--\nA" ||
		got["htmlSignature"] != "<p>A</p>" || got["mayDelete"] != true || fmt.Sprint(got["replyTo"]) != "[map[email:r@example.com name:<nil>]]" {
		t.Errorf("read back %v", got)
	}
	if after != out["newState"] {
		t.Errorf("Identity/get state %s, Identity/set newState %v", after, out["newState"])
	}
	if len(list) != 3 {
		t.Errorf("%d identities, want the login, the alias and the created one", len(list))
	}
}

// The login's identity takes a name and a signature but keeps its address and
// stays undeletable.
func TestIdentitySetUpdatesTheLoginIdentity(t *testing.T) {
	s := identityServer(t)
	login := identityID(testUser)
	out := identitySet(t, s, fmt.Sprintf(`"update":{%q:{"name":"Me","textSignature":"sig","bcc":[{"email":"b@example.com"}]}}`, login))
	if _, ok := out["updated"].(map[string]any)[login]; !ok {
		t.Fatalf("Identity/set = %v", out)
	}
	list, _ := identityList(t, s)
	got := list[login]
	if got["name"] != "Me" || got["textSignature"] != "sig" || got["email"] != testUser || got["mayDelete"] != false ||
		fmt.Sprint(got["bcc"]) != "[map[email:b@example.com name:<nil>]]" {
		t.Errorf("read back %v", got)
	}
}

func TestIdentitySetDestroysACreatedIdentity(t *testing.T) {
	s := identityServer(t)
	id := entry(identitySet(t, s, `"create":{"n":{"email":"`+aliasAddr+`"}}`), "created", "n")["id"].(string)
	out := identitySet(t, s, `"destroy":["`+id+`"]`)
	if fmt.Sprint(out["destroyed"]) != "["+id+"]" {
		t.Fatalf("Identity/set = %v", out)
	}
	if list, _ := identityList(t, s); list[id] != nil {
		t.Errorf("destroyed identity still listed: %v", list[id])
	}
}

func TestIdentitySetRefusals(t *testing.T) {
	login := identityID(testUser)
	cases := []struct {
		name, args string
		bucket, id string
		want       string
		props      []string
	}{
		{"address not the account's", `"create":{"n":{"email":"x@other.test"}}`, "notCreated", "n", "forbiddenFrom", nil},
		{"no email", `"create":{"n":{"name":"x"}}`, "notCreated", "n", "invalidProperties", []string{"email"}},
		{"server-set id", `"create":{"n":{"id":"I1","email":"` + aliasAddr + `"}}`, "notCreated", "n", "invalidProperties", []string{"id"}},
		{"server-set mayDelete", `"create":{"n":{"mayDelete":true,"email":"` + aliasAddr + `"}}`, "notCreated", "n", "invalidProperties", []string{"mayDelete"}},
		{"unknown property", `"create":{"n":{"sortOrder":0,"email":"` + aliasAddr + `"}}`, "notCreated", "n", "invalidProperties", []string{"sortOrder"}},
		{"null name", `"create":{"n":{"name":null,"email":"` + aliasAddr + `"}}`, "notCreated", "n", "invalidProperties", []string{"name"}},
		{"wrong type", `"create":{"n":{"replyTo":"r@example.com","email":"` + aliasAddr + `"}}`, "notCreated", "n", "invalidProperties", []string{"replyTo"}},
		{"email changed", fmt.Sprintf(`"update":{%q:{"email":%q}}`, login, aliasAddr), "notUpdated", login, "invalidProperties", []string{"email"}},
		{"mayDelete changed", fmt.Sprintf(`"update":{%q:{"mayDelete":true}}`, login), "notUpdated", login, "invalidProperties", []string{"mayDelete"}},
		{"update of an unknown id", `"update":{"Inope":{"name":"x"}}`, "notUpdated", "Inope", "notFound", nil},
		{"destroy of the login's identity", `"destroy":["` + login + `"]`, "notDestroyed", login, "forbidden", nil},
		{"destroy of an alias identity", `"destroy":["` + identityID(aliasAddr) + `"]`, "notDestroyed", identityID(aliasAddr), "forbidden", nil},
		{"destroy of an unknown id", `"destroy":["Inope"]`, "notDestroyed", "Inope", "notFound", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := identityServer(t)
			_, before := identityList(t, s)
			out := identitySet(t, s, tc.args)
			e := entry(out, tc.bucket, tc.id)
			if e["type"] != tc.want || (tc.props != nil && fmt.Sprint(e["properties"]) != fmt.Sprint(tc.props)) {
				t.Errorf("answered %v, want %s %v %v", out, tc.bucket, tc.want, tc.props)
			}
			if _, after := identityList(t, s); after != before {
				t.Errorf("a refused call moved the state %s -> %s", before, after)
			}
		})
	}
}

func TestIdentitySetMethodErrors(t *testing.T) {
	cases := []struct {
		name, args, want string
		max              int
	}{
		{"ifInState another state", `"ifInState":"old","create":{"n":{"email":"` + aliasAddr + `"}}`, "stateMismatch", 0},
		{"more objects than maxObjectsInSet", `"create":{"n":{"email":"` + aliasAddr + `"},"m":{"email":"` + aliasAddr + `"}}`, "requestTooLarge", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := identityServer(t)
			s.opts.Limits.MaxObjectsInSet = tc.max
			name, out := callAs(t, s, testUser, "Identity/set", `{"accountId":"`+testAccount+`",`+tc.args+`}`)
			if name != "error" || out["type"] != tc.want {
				t.Errorf("answered %s %v, want %s", name, out, tc.want)
			}
			if list, _ := identityList(t, s); len(list) != 2 {
				t.Errorf("%d identities after a refused call", len(list))
			}
		})
	}
}

// ifInState with the current state goes through.
func TestIdentitySetIfInStateCurrent(t *testing.T) {
	s := identityServer(t)
	_, state := identityList(t, s)
	out := identitySet(t, s, `"ifInState":"`+state+`","create":{"n":{"email":"`+aliasAddr+`"}}`)
	if entry(out, "created", "n")["id"] == nil {
		t.Errorf("Identity/set = %v", out)
	}
}

// A created identity sends, by its id and by its creation id in the same
// request, and its address is the one From is checked against.
func TestASubmissionUsesACreatedIdentity(t *testing.T) {
	e := newIDEnv(t)
	r := e.batch(t,
		fmt.Sprintf(`"Identity/set",{"accountId":%q,"create":{"n":{"email":%q,"name":"Second"}}},"n"`, testAccount, testUser),
		fmt.Sprintf(`"EmailSubmission/set",{"accountId":%q,"create":{"s":{"identityId":"#n","emailId":%q}}},"s"`, testAccount, e.emailID))
	if createdIDOf(r["s"], "s") == "" || len(e.relay.all()) != 1 {
		t.Fatalf("EmailSubmission/set = %v", r["s"].args)
	}
	alias, _ := entry(identitySet(t, e.s, `"create":{"a":{"email":"`+testUser+`","name":"Third"}}`), "created", "a")["id"].(string)
	if alias == "" {
		t.Fatal("no second identity for the login")
	}
	foreign, _ := entry(identitySet(t, e.s, `"create":{"f":{"email":"x@other.test"}}`), "notCreated", "f")["type"].(string)
	if foreign != "forbiddenFrom" {
		t.Errorf("an identity of another address answered %q", foreign)
	}
	out := subCall(t, e.s, "EmailSubmission/set", createSubmission(alias, e.emailID, ""))
	if entry(out, "created", "c1")["id"] == nil {
		t.Errorf("a created identity of the login did not send: %v", out)
	}
}

// A created identity whose address the account no longer has is gone from the
// list and cannot send.
func TestACreatedIdentityLeavesWithItsAddress(t *testing.T) {
	s := identityServer(t)
	id := entry(identitySet(t, s, `"create":{"n":{"email":"`+aliasAddr+`"}}`), "created", "n")["id"].(string)
	resolve := s.opts.Storage.ResolveUser
	s.opts.Storage.ResolveUser = func(u string) (*mailbox.UserInfo, error) {
		info, err := resolve(u)
		if err == nil {
			info.IdentityAliases = nil
		}
		return info, err
	}
	if list, _ := identityList(t, s); list[id] != nil {
		t.Errorf("identity of a removed alias still listed: %v", list[id])
	}
}

// A write is announced on the user's identities key, and a subscriber to the
// account hears it.
func TestAnIdentityWriteIsAnnouncedToTheAccount(t *testing.T) {
	s := identityServer(t)
	locker := lockClient(t, lockService(t))
	s.opts.Storage.Locker = locker
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events, err := locker.Subscribe(ctx, locks.AccountKey(testUser))
	if err != nil {
		t.Fatal(err)
	}
	identitySet(t, s, `"create":{"n":{"email":"`+aliasAddr+`"}}`)
	for {
		select {
		case evt := <-events:
			if evt.Resource == locks.IdentitiesKey(testUser) {
				if got := typesOfEvent(evt); !slices.Equal(got, []counters.Type{counters.Identity}) {
					t.Errorf("event %v maps to %v", evt, got)
				}
				return
			}
		case <-ctx.Done():
			t.Fatalf("no event on %s", locks.IdentitiesKey(testUser))
		}
	}
}

// A message is sent through a created identity only when its From is that
// identity's address.
func TestACreatedIdentityChecksFrom(t *testing.T) {
	s, emailID, _ := submissionServer(t, draftHeaders+"\r\nbody\r\n", &fakeRelay{}, Submission{}, aliasAddr)
	id, _ := entry(identitySet(t, s, `"create":{"n":{"email":"`+aliasAddr+`"}}`), "created", "n")["id"].(string)
	if id == "" {
		t.Fatal("no identity for the alias")
	}
	out := subCall(t, s, "EmailSubmission/set", createSubmission(id, emailID, ""))
	if entry(out, "notCreated", "c1")["type"] != "forbiddenFrom" {
		t.Errorf("From %s sent through an identity of %s: %v", testUser, aliasAddr, out)
	}
}

// A changed derived identity keeps the address userdb gives it now, not the one
// it had when it was changed.
func TestADerivedIdentityFollowsUserdb(t *testing.T) {
	s := identityServer(t)
	id := identityID(aliasAddr)
	identitySet(t, s, fmt.Sprintf(`"update":{%q:{"name":"Alias"}}`, id))
	resolve := s.opts.Storage.ResolveUser
	s.opts.Storage.ResolveUser = func(u string) (*mailbox.UserInfo, error) {
		info, err := resolve(u)
		if err == nil {
			info.IdentityAliases = []string{"A@Example.com"}
		}
		return info, err
	}
	list, _ := identityList(t, s)
	if got := list[id]; got["email"] != "A@Example.com" || got["name"] != "Alias" {
		t.Errorf("read back %v, want the userdb address with the stored name", got)
	}
}
