package jmap

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	goSmtp "github.com/emersion/go-smtp"

	"github.com/yarilomail/yarilo/internal/userstate/counters"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// sent is one message the fake relay was handed.
type sent struct {
	from       string
	rcpts      []string
	body       string
	ip         net.IP
	login, sid string
}

type fakeRelay struct {
	mu      sync.Mutex
	sent    []sent
	err     error
	okFirst int // sends that go through before err
}

func (f *fakeRelay) Send(from string, rcpts []string, body io.Reader, ip net.IP, login, sid string) error {
	b, _ := io.ReadAll(body)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil && len(f.sent) >= f.okFirst {
		return f.err
	}
	f.sent = append(f.sent, sent{from, rcpts, string(b), ip, login, sid})
	return nil
}

func (f *fakeRelay) all() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sent(nil), f.sent...)
}

const draftHeaders = "From: <u1@example.com>\r\nTo: <x@ext.test>\r\nCc: <y@ext.test>\r\nBcc: <z@ext.test>,\r\n <w@ext.test>\r\nSubject: hello\r\n"

// submissionServer serves testUser one stored message, raw, with the given
// identity aliases, sending through relay.
func submissionServer(t *testing.T, raw string, relay *fakeRelay, sub Submission, aliases ...string) (*Server, string, string) {
	t.Helper()
	s, emailID, home := storedServerWithMessageAt(t, raw, 0)
	orig := s.opts.Storage.ResolveUser
	s.opts.Storage.ResolveUser = func(u string) (*mailbox.UserInfo, error) {
		info, err := orig(u)
		if err == nil {
			info.IdentityAliases = aliases
		}
		return info, err
	}
	sub.Sender = relay
	if sub.TTL == 0 {
		sub.TTL = time.Hour
	}
	s.opts.Submission = &sub
	s.opts.Limits.Submission = sub.capability()
	return s, emailID, home
}

func subCall(t *testing.T, s *Server, method, args string) map[string]any {
	t.Helper()
	name, out := callAs(t, s, testUser, method, args)
	if name != method {
		t.Fatalf("%s answered %s %v", method, name, out)
	}
	return out
}

func createSubmission(identity, email, extra string) string {
	return `{"accountId":"` + testUser + `","create":{"c1":{"identityId":"` + identity + `","emailId":"` + email + `"` + extra + `}}}`
}

func TestIdentities(t *testing.T) {
	cases := []struct {
		name    string
		user    string
		aliases []string
		want    []string
	}{
		{"the login, then the aliases", "u1@example.com", []string{"a@example.com", "b@example.com"}, []string{"u1@example.com", "a@example.com", "b@example.com"}},
		{"an alias repeating the login in another case", "u1@example.com", []string{"U1@Example.com"}, []string{"u1@example.com"}},
		{"an alias that is no address is dropped", "u1@example.com", []string{"not an address", "Bob <b@example.com>", "c@example.com"}, []string{"u1@example.com", "c@example.com"}},
		{"a login without @ gives the aliases only", "u1", []string{"a@example.com"}, []string{"a@example.com"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, id := range identities(c.user, c.aliases) {
				got = append(got, id.Email)
				if id.ID != identityID(id.Email) || id.MayDelete || id.Name != "" {
					t.Errorf("identity %+v: want the address's id, mayDelete false, no name", id)
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("identities = %v, want %v", got, c.want)
			}
		})
	}
	if identityID("A@Example.com") != identityID("a@example.com") || !strings.HasPrefix(identityID("a@b"), "I") || len(identityID("a@b")) != 17 {
		t.Errorf("identityID %q: want I + 16 hex of the lower-cased address", identityID("a@b"))
	}
}

func TestIdentityMethods(t *testing.T) {
	s, _, _ := submissionServer(t, draftHeaders+"\r\nbody\r\n", &fakeRelay{}, Submission{}, "a@example.com")
	got := subCall(t, s, "Identity/get", `{"accountId":"`+testUser+`","ids":null}`)
	if list, _ := got["list"].([]any); len(list) != 2 {
		t.Fatalf("Identity/get = %v, want the login and the alias", got)
	}
	state, _ := got["state"].(string)
	ch := subCall(t, s, "Identity/changes", `{"accountId":"`+testUser+`","sinceState":"`+state+`"}`)
	if c, _ := ch["created"].([]any); len(c) != 0 {
		t.Errorf("Identity/changes from the current state = %v, want nothing", ch)
	}
	name, out := callAs(t, s, testUser, "Identity/changes", `{"accountId":"`+testUser+`","sinceState":"old"}`)
	if name != "error" || out["type"] != "cannotCalculateChanges" {
		t.Errorf("Identity/changes from another state answered %s %v", name, out)
	}
	set := subCall(t, s, "Identity/set", `{"accountId":"`+testUser+`","create":{"c1":{"email":"n@example.com"}},"destroy":["`+identityID(testUser)+`"]}`)
	if entry(set, "notCreated", "c1")["type"] != "forbidden" {
		t.Errorf("Identity/set = %v, want each object forbidden", set)
	}
}

func TestEmailSubmissionSends(t *testing.T) {
	relay := &fakeRelay{}
	s, emailID, _ := submissionServer(t, draftHeaders+"\r\nbody\r\n", relay, Submission{AddReceived: true, Hostname: "mx.test"})
	out := subCall(t, s, "EmailSubmission/set", createSubmission(identityID(testUser), emailID, ""))
	c := entry(out, "created", "c1")
	if c["id"] == nil || c["undoStatus"] != "final" || c["envelope"] == nil {
		t.Fatalf("created = %v, want an id, undoStatus final and the envelope", out)
	}
	got := relay.all()
	if len(got) != 1 {
		t.Fatalf("relay got %d messages, want 1", len(got))
	}
	m := got[0]
	if m.from != testUser || fmt.Sprint(m.rcpts) != "[x@ext.test y@ext.test z@ext.test w@ext.test]" {
		t.Errorf("envelope %s -> %v, want the From and To+Cc+Bcc", m.from, m.rcpts)
	}
	if strings.Contains(strings.ToLower(m.body), "bcc:") || strings.Contains(m.body, "w@ext.test") {
		t.Errorf("the relayed message still carries Bcc:\n%s", m.body)
	}
	if !strings.HasPrefix(m.body, "Received: from [203.0.113.7]\r\n\tby mx.test with HTTP;") || !strings.Contains(m.body, "Subject: hello\r\n\r\nbody") {
		t.Errorf("relayed message:\n%s", m.body)
	}
	if m.login != testUser || m.sid != "deadbeefdeadbeef" || m.ip.String() != "203.0.113.7" {
		t.Errorf("relay told login %q sid %q ip %v, want the session's", m.login, m.sid, m.ip)
	}
	get := subCall(t, s, "EmailSubmission/get", `{"accountId":"`+testUser+`","ids":null}`)
	if list, _ := get["list"].([]any); len(list) != 1 {
		t.Errorf("EmailSubmission/get = %v, want the record", get)
	}
}

func TestEmailSubmissionRefusals(t *testing.T) {
	other := "From: <other@example.com>\r\nTo: <x@ext.test>\r\nSubject: s\r\n\r\nb\r\n"
	cases := []struct {
		name  string
		raw   string
		sub   Submission
		extra string
		alias []string
		want  string
		check func(t *testing.T, e map[string]any)
	}{
		{name: "a From the identity does not allow", raw: other, want: "forbiddenFrom"},
		{name: "a given mailFrom the identity does not allow", raw: draftHeaders + "\r\nb\r\n",
			extra: `,"envelope":{"mailFrom":{"email":"other@example.com"},"rcptTo":[{"email":"x@ext.test"}]}`, want: "forbiddenMailFrom"},
		{name: "an envelope parameter other than SIZE", raw: draftHeaders + "\r\nb\r\n",
			extra: `,"envelope":{"mailFrom":{"email":"u1@example.com"},"rcptTo":[{"email":"x@ext.test","parameters":{"NOTIFY":"NEVER"}}]}`, want: "invalidProperties"},
		{name: "no recipient", raw: "From: <u1@example.com>\r\nSubject: s\r\n\r\nb\r\n", want: "noRecipients"},
		{name: "an invalid recipient", raw: draftHeaders + "\r\nb\r\n",
			extra: `,"envelope":{"mailFrom":{"email":"u1@example.com"},"rcptTo":[{"email":"not-an-address"}]}`, want: "invalidRecipients",
			check: func(t *testing.T, e map[string]any) {
				if fmt.Sprint(e["invalidRecipients"]) != "[not-an-address]" {
					t.Errorf("invalidRecipients = %v", e["invalidRecipients"])
				}
			}},
		{name: "past the recipients limit", raw: draftHeaders + "\r\nb\r\n", sub: Submission{MaxRecipients: 2}, want: "tooManyRecipients",
			check: func(t *testing.T, e map[string]any) {
				if e["maxRecipients"] != 2.0 {
					t.Errorf("maxRecipients = %v", e["maxRecipients"])
				}
			}},
		{name: "past the size", raw: draftHeaders + "\r\nb\r\n", sub: Submission{MaxSize: 20}, want: "tooLarge",
			check: func(t *testing.T, e map[string]any) {
				if e["maxSize"] != 20.0 {
					t.Errorf("maxSize = %v", e["maxSize"])
				}
			}},
		{name: "two Sender addresses", raw: "From: <u1@example.com>\r\nSender: <a@x.test>, <b@x.test>\r\nTo: <x@ext.test>\r\n\r\nb\r\n", want: "invalidEmail"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			relay := &fakeRelay{}
			s, emailID, _ := submissionServer(t, c.raw, relay, c.sub, c.alias...)
			out := subCall(t, s, "EmailSubmission/set", createSubmission(identityID(testUser), emailID, c.extra))
			e := entry(out, "notCreated", "c1")
			if e["type"] != c.want {
				t.Fatalf("notCreated c1 = %v, want %s", e, c.want)
			}
			if c.check != nil {
				c.check(t, e)
			}
			if n := len(relay.all()); n != 0 {
				t.Errorf("a refused submission reached the relay %d times", n)
			}
		})
	}
	s, _, _ := submissionServer(t, draftHeaders+"\r\nb\r\n", &fakeRelay{}, Submission{})
	for _, args := range []string{createSubmission("Ino", "x", ""), createSubmission(identityID(testUser), "nosuchemail", "")} {
		if e := entry(subCall(t, s, "EmailSubmission/set", args), "notCreated", "c1"); e["type"] != "invalidProperties" {
			t.Errorf("%s: notCreated = %v, want invalidProperties", args, e)
		}
	}
}

// A Sender the identity does not allow gives way to the identity's address
// in the generated envelope; the From is the identity's, so it is sent.
func TestEmailSubmissionMailFromFallsBackToTheIdentity(t *testing.T) {
	relay := &fakeRelay{}
	raw := "From: <u1@example.com>\r\nSender: <secretary@example.com>\r\nTo: <x@ext.test>\r\n\r\nb\r\n"
	s, emailID, _ := submissionServer(t, raw, relay, Submission{})
	subCall(t, s, "EmailSubmission/set", createSubmission(identityID(testUser), emailID, ""))
	if got := relay.all(); len(got) != 1 || got[0].from != testUser {
		t.Errorf("mailFrom = %v, want the identity's %s", got, testUser)
	}
}

func TestEmailSubmissionRelayRefusals(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantSetErr string
		wantMethod string
		wantStored int
	}{
		{"a final refusal is forbiddenToSend", &goSmtp.SMTPError{Code: 550, EnhancedCode: goSmtp.EnhancedCode{5, 7, 1}, Message: "Sender address rejected"}, "forbiddenToSend", "", 0},
		{"a temporary refusal is serverFail", &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 4, 0}, Message: "Failed to connect"}, "", "serverFail", 0},
		{"a transport error is serverFail", fmt.Errorf("dial: i/o timeout"), "", "serverFail", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, emailID, _ := submissionServer(t, draftHeaders+"\r\nb\r\n", &fakeRelay{err: c.err}, Submission{})
			name, out := callAs(t, s, testUser, "EmailSubmission/set", createSubmission(identityID(testUser), emailID, ""))
			switch {
			case c.wantMethod != "":
				if name != "error" || out["type"] != c.wantMethod {
					t.Fatalf("answered %s %v, want error %s", name, out, c.wantMethod)
				}
			default:
				e := entry(out, "notCreated", "c1")
				if e["type"] != c.wantSetErr || !strings.Contains(fmt.Sprint(e["description"]), "550 5.7.1 Sender address rejected") {
					t.Fatalf("notCreated c1 = %v, want %s with the relay's reply", e, c.wantSetErr)
				}
			}
			get := subCall(t, s, "EmailSubmission/get", `{"accountId":"`+testUser+`","ids":null}`)
			if list, _ := get["list"].([]any); len(list) != c.wantStored {
				t.Errorf("records = %v, want %d", list, c.wantStored)
			}
		})
	}
}

// A temporary refusal after a send in the same call leaves the sent one in
// created and fails only this create and the ones not yet attempted.
func TestEmailSubmissionTemporaryRefusalAfterASend(t *testing.T) {
	relay := &fakeRelay{err: &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 4, 0}, Message: "Failed to connect"}, okFirst: 1}
	s, emailID, _ := submissionServer(t, draftHeaders+"\r\nb\r\n", relay, Submission{})
	using, _ := json.Marshal(declaredCapabilities(s.opts.Limits))
	one := fmt.Sprintf(`{"identityId":%q,"emailId":%q}`, identityID(testUser), emailID)
	body := fmt.Sprintf(`{"using":%s,"methodCalls":[["EmailSubmission/set",{"accountId":%q,"create":{"c1":%s,"c2":%s,"c3":%s},"onSuccessUpdateEmail":{"#c1":{"keywords/$seen":true}}},"0"]]}`,
		using, testUser, one, one, one)
	w := postAPIRaw(t, s, body)
	var resp struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || len(resp.MethodResponses) != 2 {
		t.Fatalf("responses = %s, want EmailSubmission/set then Email/set", w.Body)
	}
	var name string
	var out map[string]any
	_ = json.Unmarshal(resp.MethodResponses[0][0], &name)
	_ = json.Unmarshal(resp.MethodResponses[0][1], &out)
	if name != "EmailSubmission/set" || entry(out, "created", "c1")["id"] == nil {
		t.Fatalf("first response = %s %v, want c1 created", name, out)
	}
	for _, cid := range []string{"c2", "c3"} {
		if e := entry(out, "notCreated", cid); e["type"] != "serverFail" || e["description"] == nil {
			t.Errorf("notCreated %s = %v, want serverFail with a description", cid, e)
		}
	}
	if n := len(relay.all()); n != 1 {
		t.Errorf("relay sends = %d, want 1", n)
	}
	get := subCall(t, s, "EmailSubmission/get", `{"accountId":"`+testUser+`","ids":null}`)
	if list, _ := get["list"].([]any); len(list) != 1 {
		t.Errorf("records = %v, want c1's", list)
	}
}

// onSuccessUpdateEmail runs one implicit Email/set, answered right after.
func TestEmailSubmissionOnSuccessUpdatesTheEmail(t *testing.T) {
	s, emailID, _ := submissionServer(t, draftHeaders+"\r\nb\r\n", &fakeRelay{}, Submission{})
	using, _ := json.Marshal(declaredCapabilities(s.opts.Limits))
	body := fmt.Sprintf(`{"using":%s,"methodCalls":[["EmailSubmission/set",{"accountId":%q,"create":{"c1":{"identityId":%q,"emailId":%q}},"onSuccessUpdateEmail":{"#c1":{"keywords/$draft":null,"keywords/$seen":true}}},"0"]]}`,
		using, testUser, identityID(testUser), emailID)
	w := postAPIRaw(t, s, body)
	var resp struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || len(resp.MethodResponses) != 2 {
		t.Fatalf("responses = %s, want EmailSubmission/set then Email/set", w.Body)
	}
	var second string
	_ = json.Unmarshal(resp.MethodResponses[1][0], &second)
	if second != "Email/set" || !strings.Contains(string(resp.MethodResponses[1][1]), emailID) || string(resp.MethodResponses[1][2]) != `"0"` {
		t.Errorf("second response = %s", resp.MethodResponses[1])
	}
	got := subCall(t, s, "Email/get", `{"accountId":"`+testUser+`","ids":["`+emailID+`"],"properties":["keywords"]}`)
	if !strings.Contains(fmt.Sprint(got["list"]), "$seen:true") {
		t.Errorf("the email after the implicit Email/set = %v", got["list"])
	}
}

func TestEmailSubmissionRecords(t *testing.T) {
	s, emailID, home := submissionServer(t, draftHeaders+"\r\nb\r\n", &fakeRelay{}, Submission{})
	out := subCall(t, s, "EmailSubmission/set", createSubmission(identityID(testUser), emailID, ""))
	id, _ := entry(out, "created", "c1")["id"].(string)

	upd := subCall(t, s, "EmailSubmission/set", `{"accountId":"`+testUser+`","update":{"`+id+`":{"undoStatus":"canceled"}}}`)
	if entry(upd, "notUpdated", id)["type"] != "cannotUnsend" {
		t.Errorf("cancel = %v, want cannotUnsend", upd)
	}
	q := subCall(t, s, "EmailSubmission/query", `{"accountId":"`+testUser+`","filter":{"emailIds":["`+emailID+`"],"undoStatus":"final"},"sort":[{"property":"sentAt"}]}`)
	if fmt.Sprint(q["ids"]) != "["+id+"]" {
		t.Errorf("query = %v, want [%s]", q["ids"], id)
	}

	// A record past the TTL is gone at the next read.
	path := filepath.Join(home, submissionFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no record file: %v", err)
	}
	var recs submissionRecords
	_ = json.Unmarshal(raw, &recs)
	recs.Records[0].SendAt = time.Now().Add(-2 * time.Hour)
	b, _ := json.Marshal(recs)
	_ = os.WriteFile(path, b, 0o600)
	get := subCall(t, s, "EmailSubmission/get", `{"accountId":"`+testUser+`","ids":null}`)
	if list, _ := get["list"].([]any); len(list) != 0 {
		t.Errorf("a record past the TTL is still listed: %v", list)
	}

	out = subCall(t, s, "EmailSubmission/set", createSubmission(identityID(testUser), emailID, ""))
	id2, _ := entry(out, "created", "c1")["id"].(string)
	del := subCall(t, s, "EmailSubmission/set", `{"accountId":"`+testUser+`","destroy":["`+id2+`"]}`)
	if d, _ := del["destroyed"].([]any); len(d) != 1 {
		t.Errorf("destroy = %v", del)
	}
	after := subCall(t, s, "EmailSubmission/get", `{"accountId":"`+testUser+`","ids":["`+id2+`"]}`)
	if list, _ := after["list"].([]any); len(list) != 0 {
		t.Errorf("a destroyed record is still listed: %v", list)
	}
}

// jmap_submission_ttl_seconds 0 keeps no record; the message is still sent.
func TestEmailSubmissionWithoutRecords(t *testing.T) {
	relay := &fakeRelay{}
	s, emailID, home := submissionServer(t, draftHeaders+"\r\nb\r\n", relay, Submission{})
	s.opts.Submission.TTL = 0
	subCall(t, s, "EmailSubmission/set", createSubmission(identityID(testUser), emailID, ""))
	get := subCall(t, s, "EmailSubmission/get", `{"accountId":"`+testUser+`","ids":null}`)
	if list, _ := get["list"].([]any); len(list) != 0 || len(relay.all()) != 1 {
		t.Errorf("records %v, sent %d: want none kept and one sent", list, len(relay.all()))
	}
	if _, err := os.Stat(filepath.Join(home, submissionFile)); !os.IsNotExist(err) {
		t.Errorf("a record file was written with TTL 0: %v", err)
	}
}

func TestSubmissionCapability(t *testing.T) {
	if got := (&Submission{MaxSize: 1000}).capability(); fmt.Sprint(got.SubmissionExtensions) != "map[SIZE:[1000]]" || got.MaxDelayedSend != 0 {
		t.Errorf("capability = %+v", got)
	}
	if got := (&Submission{}).capability(); len(got.SubmissionExtensions) != 0 {
		t.Errorf("no size limit: extensions %v, want none", got.SubmissionExtensions)
	}
	if New(Options{}).opts.Limits.Submission != nil {
		t.Error("submission advertised without a relay")
	}
	if New(Options{Submission: &Submission{}}).opts.Limits.Submission == nil {
		t.Error("submission not advertised with one")
	}

}

// Without a relay the capability is refused at the door, not method by method.
func TestSubmissionCapabilityIsRefusedWithoutARelay(t *testing.T) {
	s, _, _ := storedServerWithMessageAt(t, "Subject: seed\r\n\r\nseed\r\n", 0)
	body := `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:submission"],"methodCalls":[["Identity/get",{"accountId":"` + testUser + `"},"c0"]]}`
	if w := postAPIRaw(t, s, body); w.Code != 400 || !strings.Contains(w.Body.String(), "unknownCapability") {
		t.Errorf("status %d %s, want 400 unknownCapability", w.Code, w.Body)
	}
}

// A kept or dropped record moves EmailSubmission; a read that changes nothing
// does not.
func TestEmailSubmissionRecordsMoveTheirType(t *testing.T) {
	s, emailID, home := submissionServer(t, draftHeaders+"\r\nb\r\n", &fakeRelay{}, Submission{})
	var id string
	steps := []struct {
		name string
		call func()
		want []counters.Type
	}{
		{"create", func() {
			out := subCall(t, s, "EmailSubmission/set", createSubmission(identityID(testUser), emailID, ""))
			id, _ = entry(out, "created", "c1")["id"].(string)
		}, []counters.Type{counters.EmailSubmission}},
		{"get", func() { subCall(t, s, "EmailSubmission/get", `{"accountId":"`+testUser+`","ids":null}`) }, nil},
		{"destroy", func() {
			subCall(t, s, "EmailSubmission/set", `{"accountId":"`+testUser+`","destroy":["`+id+`"]}`)
		}, []counters.Type{counters.EmailSubmission}},
	}
	for _, st := range steps {
		before, _ := counters.Read(counters.Path(home))
		st.call()
		after, _ := counters.Read(counters.Path(home))
		if got := after.ChangedAfter(before.Highest); !reflect.DeepEqual(got, st.want) {
			t.Errorf("%s moved %v, want %v", st.name, got, st.want)
		}
	}
}
