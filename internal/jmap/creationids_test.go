package jmap

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/fts/language"
	"github.com/yarilomail/yarilo/internal/sieve"
)

// idEnv is one server with a stored message, a relay and a script store.
type idEnv struct {
	s       *Server
	inbox   string
	emailID string
	relay   *fakeRelay
	blob    string
}

func newIDEnv(t *testing.T) idEnv {
	t.Helper()
	relay := &fakeRelay{}
	s, emailID, _ := submissionServer(t, draftHeaders+"\r\nb\r\n", relay, Submission{})
	s.opts.Sieve = &SieveScripts{Store: sieve.NewScriptStore("fs", "", nil, nil), MaxRedirects: 32, Implementation: "yarilo test"}
	s.opts.Limits.Sieve = s.opts.Sieve.session()
	s.opts.Limits.MaxSizeUpload = 4096
	s.opts.UploadTTL = time.Hour
	return idEnv{s: s, inbox: inboxID(t, s), emailID: emailID, relay: relay, blob: uploadAs(t, s, testUser, keepScript)}
}

type callResult struct {
	name string
	args map[string]any
}

// batch runs calls, each `"Name", {args}, "callId"`, as one request.
func (e idEnv) batch(t *testing.T, calls ...string) map[string]callResult {
	t.Helper()
	return e.batchWith(t, "", calls...)
}

// batchWith sends createdIds too, for ids no method here creates.
func (e idEnv) batchWith(t *testing.T, createdIDs string, calls ...string) map[string]callResult {
	t.Helper()
	using, _ := json.Marshal(declaredCapabilities(e.s.opts.Limits))
	var b strings.Builder
	for i, c := range calls {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("[" + c + "]")
	}
	extra := ""
	if createdIDs != "" {
		extra = `,"createdIds":` + createdIDs
	}
	w := postAPIRaw(t, e.s, fmt.Sprintf(`{"using":%s,"methodCalls":[%s]%s}`, using, b.String(), extra))
	var resp struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response %s: %v", w.Body, err)
	}
	out := map[string]callResult{}
	for _, r := range resp.MethodResponses {
		var name, id string
		var args map[string]any
		_ = json.Unmarshal(r[0], &name)
		_ = json.Unmarshal(r[1], &args)
		_ = json.Unmarshal(r[2], &id)
		if _, seen := out[id]; !seen {
			out[id] = callResult{name, args}
		}
	}
	return out
}

func (e idEnv) mailbox(cid, name, parent string) string {
	p := ""
	if parent != "" {
		p = fmt.Sprintf(`,"parentId":%q`, parent)
	}
	return fmt.Sprintf(`"Mailbox/set",{"accountId":%q,"create":{%q:{"name":%q%s}}},"m-%s"`, testUser, cid, name, p, cid)
}

func (e idEnv) draft(cid, box string) string {
	return fmt.Sprintf(`"Email/set",{"accountId":%q,"create":{%q:{"mailboxIds":{%q:true},"keywords":{"$draft":true},`+
		`"from":[{"email":%q}],"to":[{"email":"x@ext.test"}],"subject":"hi","textBody":[{"partId":"1","type":"text/plain"}],`+
		`"bodyValues":{"1":{"value":"b"}}}}},"e-%s"`, testUser, cid, box, testUser, cid)
}

func (e idEnv) submission(cid, email string) string {
	return fmt.Sprintf(`"EmailSubmission/set",{"accountId":%q,"create":{%q:{"identityId":%q,"emailId":%q}}},"s-%s"`,
		testUser, cid, identityID(testUser), email, cid)
}

func (e idEnv) script(cid string) string {
	return fmt.Sprintf(`"SieveScript/set",{"accountId":%q,"create":{%q:{"name":%q,"blobId":%q}}},"v-%s"`, testUser, cid, cid, e.blob, cid)
}

func createdIDOf(r callResult, cid string) string {
	id, _ := entry(r.args, "created", cid)["id"].(string)
	return id
}

func listIDs(r callResult, field string) []string {
	var out []string
	list, _ := r.args["list"].([]any)
	for _, o := range list {
		m, _ := o.(map[string]any)
		out = append(out, fmt.Sprint(m[field]))
	}
	return out
}

func strs(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		out = append(out, fmt.Sprint(x))
	}
	return out
}

func keys(v any) []string {
	m, _ := v.(map[string]any)
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// idArguments is every argument of every method that takes an id a client may
// give as "#<creation id>" (RFC 8620 §5.3); each pair has a row below.
var idArguments = map[string][]string{
	"Mailbox/get":           {"ids"},
	"Mailbox/set":           {"update", "destroy", "parentId"},
	"Mailbox/query":         {"filter.parentId"},
	"Email/get":             {"ids"},
	"Email/set":             {"update", "destroy", "mailboxIds", "update mailboxIds", "update mailboxIds/<id>"},
	"Email/import":          {"mailboxIds"},
	"Email/query":           {"filter.inMailbox", "filter.inMailboxOtherThan"},
	"Thread/get":            {"ids"},
	"SearchSnippet/get":     {"emailIds"},
	"Identity/get":          {"ids"},
	"Identity/set":          {"update", "destroy"},
	"EmailSubmission/get":   {"ids"},
	"EmailSubmission/set":   {"update", "destroy", "emailId", "identityId"},
	"EmailSubmission/query": {"filter.emailIds", "filter.threadIds", "filter.identityIds"},
	"SieveScript/get":       {"ids"},
	"SieveScript/set":       {"update", "destroy", "onSuccessActivateScript"},
	"Quota/get":             {"ids"},
}

// noIDArguments names the methods that take no such id, and why.
var noIDArguments = map[string]string{
	"Core/echo":                    "echoes its arguments",
	"Mailbox/changes":              "sinceState only",
	"Email/changes":                "sinceState only",
	"Thread/changes":               "sinceState only",
	"Identity/changes":             "sinceState only",
	"EmailSubmission/changes":      "sinceState only",
	"Quota/changes":                "sinceState only",
	"Mailbox/queryChanges":         "always cannotCalculateChanges",
	"Email/queryChanges":           "always cannotCalculateChanges",
	"EmailSubmission/queryChanges": "always cannotCalculateChanges",
	"Quota/queryChanges":           "always cannotCalculateChanges",
	"Quota/query":                  "filters on names and types",
	"SieveScript/query":            "filters on name and isActive",
	"SieveScript/validate":         "blobId is an upload's; no method here creates blobs under a creation id",
}

type idRow struct {
	method, arg string
	run         func(t *testing.T, e idEnv) error
}

func idRows() []idRow {
	want := func(ok bool, format string, a ...any) error {
		if ok {
			return nil
		}
		return fmt.Errorf(format, a...)
	}
	return []idRow{
		{"Mailbox/get", "ids", func(t *testing.T, e idEnv) error {
			r := e.batch(t, e.mailbox("b", "Fresh", ""), fmt.Sprintf(`"Mailbox/get",{"accountId":%q,"ids":["#b","#nope"],"properties":["name"]},"g"`, testUser))
			return want(slices.Equal(listIDs(r["g"], "name"), []string{"Fresh"}) && slices.Equal(strs(r["g"].args["notFound"]), []string{"#nope"}),
				"Mailbox/get = %v", r["g"].args)
		}},
		{"Mailbox/set", "update", func(t *testing.T, e idEnv) error {
			r := e.batch(t, e.mailbox("b", "Fresh", ""), fmt.Sprintf(`"Mailbox/set",{"accountId":%q,"update":{"#b":{"name":"Renamed"}}},"u"`, testUser))
			id := createdIDOf(r["m-b"], "b")
			return want(id != "" && slices.Equal(keys(r["u"].args["updated"]), []string{id}), "update = %v, want %s updated", r["u"].args, id)
		}},
		{"Mailbox/set", "destroy", func(t *testing.T, e idEnv) error {
			r := e.batch(t, e.mailbox("b", "Fresh", ""), fmt.Sprintf(`"Mailbox/set",{"accountId":%q,"destroy":["#b"]},"x"`, testUser))
			id := createdIDOf(r["m-b"], "b")
			return want(id != "" && slices.Equal(strs(r["x"].args["destroyed"]), []string{id}), "destroy = %v, want %s", r["x"].args, id)
		}},
		{"Mailbox/set", "parentId", func(t *testing.T, e idEnv) error {
			r := e.batch(t, e.mailbox("b", "Fresh", ""), e.mailbox("c", "Child", "#b"),
				fmt.Sprintf(`"Mailbox/get",{"accountId":%q,"ids":["#c"],"properties":["parentId"]},"g"`, testUser))
			id := createdIDOf(r["m-b"], "b")
			return want(id != "" && slices.Equal(listIDs(r["g"], "parentId"), []string{id}), "child = %v (create %v), want parent %s", r["g"].args, r["m-c"].args, id)
		}},
		{"Mailbox/query", "filter.parentId", func(t *testing.T, e idEnv) error {
			r := e.batch(t, e.mailbox("b", "Fresh", ""), e.mailbox("c", "Child", "#b"),
				fmt.Sprintf(`"Mailbox/query",{"accountId":%q,"filter":{"parentId":"#b"}},"q"`, testUser))
			id := createdIDOf(r["m-c"], "c")
			return want(id != "" && slices.Equal(strs(r["q"].args["ids"]), []string{id}), "query = %v, want [%s]", r["q"].args, id)
		}},
		{"Email/set", "mailboxIds", func(t *testing.T, e idEnv) error {
			r := e.batch(t, e.mailbox("b", "Fresh", ""), e.draft("d", "#b"),
				fmt.Sprintf(`"Email/get",{"accountId":%q,"ids":["#d"],"properties":["mailboxIds"]},"g"`, testUser))
			id := createdIDOf(r["m-b"], "b")
			list, _ := r["g"].args["list"].([]any)
			return want(id != "" && len(list) == 1 && slices.Equal(keys(list[0].(map[string]any)["mailboxIds"]), []string{id}),
				"draft = %v (create %v), want in %s", r["g"].args, r["e-d"].args, id)
		}},
		{"Email/set", "update", func(t *testing.T, e idEnv) error {
			r := e.batch(t, e.draft("d", e.inbox), fmt.Sprintf(`"Email/set",{"accountId":%q,"update":{"#d":{"keywords/$seen":true}}},"u"`, testUser))
			id := createdIDOf(r["e-d"], "d")
			return want(id != "" && slices.Equal(keys(r["u"].args["updated"]), []string{id}), "update = %v, want %s", r["u"].args, id)
		}},
		{"Email/set", "update mailboxIds", func(t *testing.T, e idEnv) error {
			r := e.batch(t, e.mailbox("b", "Fresh", ""), fmt.Sprintf(`"Email/set",{"accountId":%q,"update":{%q:{"mailboxIds":{"#b":true}}}},"u"`, testUser, e.emailID))
			id := createdIDOf(r["m-b"], "b")
			return want(id != "" && slices.Equal(where(t, e.s, e.emailID), []string{"Fresh"}), "update = %v, the email is in %v, want [Fresh]", r["u"].args, where(t, e.s, e.emailID))
		}},
		{"Email/set", "update mailboxIds/<id>", func(t *testing.T, e idEnv) error {
			r := e.batch(t, e.mailbox("b", "Fresh", ""), fmt.Sprintf(`"Email/set",{"accountId":%q,"update":{%q:{"mailboxIds/#b":true}}},"u"`, testUser, e.emailID))
			id := createdIDOf(r["m-b"], "b")
			in := where(t, e.s, e.emailID)
			return want(id != "" && slices.Equal(in, []string{"Fresh", "INBOX"}), "update = %v, the email is in %v, want Fresh and INBOX", r["u"].args, in)
		}},
		{"Email/set", "destroy", func(t *testing.T, e idEnv) error {
			r := e.batch(t, e.draft("d", e.inbox), fmt.Sprintf(`"Email/set",{"accountId":%q,"destroy":["#d"]},"x"`, testUser))
			id := createdIDOf(r["e-d"], "d")
			return want(id != "" && slices.Equal(strs(r["x"].args["destroyed"]), []string{id}), "destroy = %v, want %s", r["x"].args, id)
		}},
		{"Email/import", "mailboxIds", func(t *testing.T, e idEnv) error {
			r := e.batch(t, e.mailbox("b", "Fresh", ""),
				fmt.Sprintf(`"Email/import",{"accountId":%q,"emails":{"i":{"blobId":%q,"mailboxIds":{"#b":true}}}},"i"`, testUser, e.emailID),
				fmt.Sprintf(`"Email/get",{"accountId":%q,"ids":["#i"],"properties":["id"]},"g"`, testUser))
			box, id := createdIDOf(r["m-b"], "b"), createdIDOf(r["i"], "i")
			return want(box != "" && id != "" && slices.Equal(where(t, e.s, id), []string{"Fresh"}) && slices.Equal(listIDs(r["g"], "id"), []string{id}),
				"import = %v, the email is in %v, want [Fresh]; #i = %v", r["i"].args, where(t, e.s, id), r["g"].args)
		}},
		{"Email/get", "ids", func(t *testing.T, e idEnv) error {
			r := e.batch(t, e.draft("d", e.inbox), fmt.Sprintf(`"Email/get",{"accountId":%q,"ids":["#d","#nope"],"properties":["id"]},"g"`, testUser))
			id := createdIDOf(r["e-d"], "d")
			return want(id != "" && slices.Equal(listIDs(r["g"], "id"), []string{id}) && slices.Equal(strs(r["g"].args["notFound"]), []string{"#nope"}),
				"Email/get = %v", r["g"].args)
		}},
		{"Email/query", "filter.inMailbox", func(t *testing.T, e idEnv) error {
			r := e.batch(t, e.mailbox("b", "Fresh", ""), e.draft("d", "#b"), fmt.Sprintf(`"Email/query",{"accountId":%q,"filter":{"inMailbox":"#b"}},"q"`, testUser))
			id := createdIDOf(r["e-d"], "d")
			return want(id != "" && slices.Equal(strs(r["q"].args["ids"]), []string{id}), "query = %v, want [%s]", r["q"].args, id)
		}},
		{"Email/query", "filter.inMailboxOtherThan", func(t *testing.T, e idEnv) error {
			r := e.batch(t, e.mailbox("b", "Fresh", ""), e.draft("d", "#b"), fmt.Sprintf(`"Email/query",{"accountId":%q,"filter":{"inMailboxOtherThan":["#b"]}},"q"`, testUser))
			return want(slices.Equal(strs(r["q"].args["ids"]), []string{e.emailID}), "query = %v, want only [%s]", r["q"].args, e.emailID)
		}},
		{"Thread/get", "ids", func(t *testing.T, e idEnv) error {
			r := e.batch(t, e.draft("d", e.inbox), fmt.Sprintf(`"Thread/get",{"accountId":%q,"ids":["#d"]},"g"`, testUser))
			th, _ := entry(r["e-d"].args, "created", "d")["threadId"].(string)
			return want(th != "" && slices.Equal(listIDs(r["g"], "id"), []string{th}), "Thread/get = %v, want %s", r["g"].args, th)
		}},
		{"SearchSnippet/get", "emailIds", func(t *testing.T, e idEnv) error {
			chain, err := language.NewMultiChain([]string{"english"}, nil, nil, language.DefaultTokenMaxLen, language.DefaultAddressMaxLen, 10)
			if err != nil {
				t.Fatalf("chain: %v", err)
			}
			e.s.opts.FTS = &FTS{Client: &stubFTS{}, Chain: chain, MaxConns: 4, MaxFolders: 8}
			r := e.batch(t, e.draft("d", e.inbox), fmt.Sprintf(`"SearchSnippet/get",{"accountId":%q,"emailIds":["#d","#nope"]},"g"`, testUser))
			id := createdIDOf(r["e-d"], "d")
			return want(id != "" && slices.Equal(listIDs(r["g"], "emailId"), []string{id}) && slices.Equal(strs(r["g"].args["notFound"]), []string{"#nope"}),
				"SearchSnippet/get = %v", r["g"].args)
		}},
		{"Identity/get", "ids", func(t *testing.T, e idEnv) error {
			id := identityID(testUser)
			r := e.batchWith(t, fmt.Sprintf(`{"i":%q}`, id), fmt.Sprintf(`"Identity/get",{"accountId":%q,"ids":["#i","#nope"]},"g"`, testUser))
			return want(slices.Equal(listIDs(r["g"], "id"), []string{id}) && slices.Equal(strs(r["g"].args["notFound"]), []string{"#nope"}), "Identity/get = %v", r["g"].args)
		}},
		{"Identity/set", "update", func(t *testing.T, e idEnv) error {
			id := identityID(testUser)
			r := e.batchWith(t, fmt.Sprintf(`{"i":%q}`, id), fmt.Sprintf(`"Identity/set",{"accountId":%q,"update":{"#i":{"name":"x"}}},"u"`, testUser))
			return want(slices.Equal(keys(r["u"].args["notUpdated"]), []string{id}), "Identity/set = %v, want %s refused", r["u"].args, id)
		}},
		{"Identity/set", "destroy", func(t *testing.T, e idEnv) error {
			id := identityID(testUser)
			r := e.batchWith(t, fmt.Sprintf(`{"i":%q}`, id), fmt.Sprintf(`"Identity/set",{"accountId":%q,"destroy":["#i"]},"x"`, testUser))
			return want(slices.Equal(keys(r["x"].args["notDestroyed"]), []string{id}), "Identity/set = %v, want %s refused", r["x"].args, id)
		}},
		{"EmailSubmission/set", "emailId", func(t *testing.T, e idEnv) error {
			r := e.batch(t, e.draft("d", e.inbox), e.submission("s", "#d"))
			sent := e.relay.all()
			return want(createdIDOf(r["s-s"], "s") != "" && len(sent) == 1 && strings.Contains(sent[0].body, "Subject: hi"),
				"EmailSubmission/set = %v, relay got %d", r["s-s"].args, len(sent))
		}},
		{"EmailSubmission/set", "identityId", func(t *testing.T, e idEnv) error {
			r := e.batchWith(t, fmt.Sprintf(`{"i":%q}`, identityID(testUser)),
				fmt.Sprintf(`"EmailSubmission/set",{"accountId":%q,"create":{"s":{"identityId":"#i","emailId":%q}}},"s"`, testUser, e.emailID))
			return want(createdIDOf(r["s"], "s") != "" && len(e.relay.all()) == 1, "EmailSubmission/set = %v", r["s"].args)
		}},
		{"EmailSubmission/set", "update", func(t *testing.T, e idEnv) error {
			r := e.batch(t, e.submission("s", e.emailID), fmt.Sprintf(`"EmailSubmission/set",{"accountId":%q,"update":{"#s":{"undoStatus":"canceled"}}},"u"`, testUser))
			id := createdIDOf(r["s-s"], "s")
			return want(id != "" && entry(r["u"].args, "notUpdated", id)["type"] == "cannotUnsend", "update = %v, want %s cannotUnsend", r["u"].args, id)
		}},
		{"EmailSubmission/set", "destroy", func(t *testing.T, e idEnv) error {
			r := e.batch(t, e.submission("s", e.emailID), fmt.Sprintf(`"EmailSubmission/set",{"accountId":%q,"destroy":["#s"]},"x"`, testUser))
			id := createdIDOf(r["s-s"], "s")
			return want(id != "" && slices.Equal(strs(r["x"].args["destroyed"]), []string{id}), "destroy = %v, want %s", r["x"].args, id)
		}},
		{"EmailSubmission/get", "ids", func(t *testing.T, e idEnv) error {
			r := e.batch(t, e.submission("s", e.emailID), fmt.Sprintf(`"EmailSubmission/get",{"accountId":%q,"ids":["#s"]},"g"`, testUser))
			id := createdIDOf(r["s-s"], "s")
			return want(id != "" && slices.Equal(listIDs(r["g"], "id"), []string{id}), "get = %v, want %s", r["g"].args, id)
		}},
		{"EmailSubmission/query", "filter.emailIds", func(t *testing.T, e idEnv) error {
			r := e.batch(t, e.draft("d", e.inbox), e.submission("s", "#d"), e.submission("o", e.emailID),
				fmt.Sprintf(`"EmailSubmission/query",{"accountId":%q,"filter":{"emailIds":["#d"]}},"q"`, testUser))
			id := createdIDOf(r["s-s"], "s")
			return want(id != "" && slices.Equal(strs(r["q"].args["ids"]), []string{id}), "query = %v, want [%s]", r["q"].args, id)
		}},
		{"EmailSubmission/query", "filter.threadIds", func(t *testing.T, e idEnv) error {
			r := e.batch(t, e.draft("d", e.inbox), e.submission("s", "#d"), e.submission("o", e.emailID),
				fmt.Sprintf(`"EmailSubmission/query",{"accountId":%q,"filter":{"threadIds":["#d"]}},"q"`, testUser))
			id := createdIDOf(r["s-s"], "s")
			return want(id != "" && slices.Equal(strs(r["q"].args["ids"]), []string{id}), "query = %v, want [%s]", r["q"].args, id)
		}},
		{"EmailSubmission/query", "filter.identityIds", func(t *testing.T, e idEnv) error {
			r := e.batchWith(t, fmt.Sprintf(`{"i":%q}`, identityID(testUser)), e.submission("s", e.emailID),
				fmt.Sprintf(`"EmailSubmission/query",{"accountId":%q,"filter":{"identityIds":["#i"]}},"q"`, testUser))
			id := createdIDOf(r["s-s"], "s")
			return want(id != "" && slices.Equal(strs(r["q"].args["ids"]), []string{id}), "query = %v, want [%s]", r["q"].args, id)
		}},
		{"SieveScript/get", "ids", func(t *testing.T, e idEnv) error {
			r := e.batch(t, e.script("v"), fmt.Sprintf(`"SieveScript/get",{"accountId":%q,"ids":["#v"]},"g"`, testUser))
			return want(slices.Equal(listIDs(r["g"], "name"), []string{"v"}), "get = %v", r["g"].args)
		}},
		{"SieveScript/set", "update", func(t *testing.T, e idEnv) error {
			r := e.batch(t, e.script("v"), fmt.Sprintf(`"SieveScript/set",{"accountId":%q,"update":{"#v":{"name":"w"}}},"u"`, testUser))
			id := createdIDOf(r["v-v"], "v")
			return want(id != "" && slices.Equal(keys(r["u"].args["updated"]), []string{id}), "update = %v, want %s", r["u"].args, id)
		}},
		{"SieveScript/set", "destroy", func(t *testing.T, e idEnv) error {
			r := e.batch(t, e.script("v"), fmt.Sprintf(`"SieveScript/set",{"accountId":%q,"destroy":["#v"]},"x"`, testUser))
			id := createdIDOf(r["v-v"], "v")
			return want(id != "" && slices.Equal(strs(r["x"].args["destroyed"]), []string{id}), "destroy = %v, want %s", r["x"].args, id)
		}},
		{"SieveScript/set", "onSuccessActivateScript", func(t *testing.T, e idEnv) error {
			r := e.batch(t, e.script("v"), fmt.Sprintf(`"SieveScript/set",{"accountId":%q,"onSuccessActivateScript":"#v"},"a"`, testUser),
				fmt.Sprintf(`"SieveScript/get",{"accountId":%q,"ids":["#v"],"properties":["isActive"]},"g"`, testUser))
			return want(slices.Equal(listIDs(r["g"], "isActive"), []string{"true"}), "after activation %v (set %v)", r["g"].args, r["a"].args)
		}},
		{"Quota/get", "ids", func(t *testing.T, e idEnv) error {
			r := e.batchWith(t, `{"q":"Qnone"}`, fmt.Sprintf(`"Quota/get",{"accountId":%q,"ids":["#q"]},"g"`, testUser))
			return want(slices.Equal(strs(r["g"].args["notFound"]), []string{"Qnone"}), "Quota/get = %v, want #q looked up as Qnone", r["g"].args)
		}},
	}
}

func TestCreationIDsReachEveryIDArgument(t *testing.T) {
	for _, row := range idRows() {
		t.Run(row.method+" "+row.arg, func(t *testing.T) {
			if err := row.run(t, newIDEnv(t)); err != nil {
				t.Error(err)
			}
		})
	}
}

// A method or id argument added without a row here fails, so none is left
// taking "#<creation id>" as a literal id.
func TestEveryIDArgumentHasACreationIDRow(t *testing.T) {
	e := newIDEnv(t)
	for name := range e.s.registry(&lazyStore{storage: e.s.opts.Storage, user: testUser}, testUser) {
		_, args := idArguments[name]
		_, none := noIDArguments[name]
		if args == none {
			t.Errorf("%s: name its id arguments in idArguments or why it has none in noIDArguments", name)
		}
	}
	covered := map[string]bool{}
	for _, r := range idRows() {
		covered[r.method+" "+r.arg] = true
	}
	for m, args := range idArguments {
		for _, a := range args {
			if !covered[m+" "+a] {
				t.Errorf("%s %s has no row", m, a)
			}
		}
	}
}

// A creation id reused in a request names the most recently created item.
func TestReusedCreationIDNamesTheLatest(t *testing.T) {
	e := newIDEnv(t)
	r := e.batch(t, e.mailbox("b", "First", ""), e.mailbox("b", "Second", ""),
		fmt.Sprintf(`"Mailbox/get",{"accountId":%q,"ids":["#b"],"properties":["name"]},"g"`, testUser))
	if got := listIDs(r["g"], "name"); !slices.Equal(got, []string{"Second"}) {
		t.Errorf("#b = %v, want Second", got)
	}
}
