package jmap

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-message"

	"github.com/yarilomail/yarilo/internal/userstate/threads"
)

func createCall(t *testing.T, s *Server, email string) map[string]any {
	t.Helper()
	name, out := callAs(t, s, testUser, "Email/set", fmt.Sprintf(`{"accountId":%q,"create":{"c1":%s}}`, testAccount, email))
	if name != "Email/set" {
		t.Fatalf("answered %s %v", name, out)
	}
	return out
}

func createdID(t *testing.T, out map[string]any) string {
	t.Helper()
	created, _ := out["created"].(map[string]any)
	c1, _ := created["c1"].(map[string]any)
	id, _ := c1["id"].(string)
	if id == "" {
		t.Fatalf("not created: %v", out["notCreated"])
	}
	return id
}

// rawOf downloads a created Email as its owner would.
func rawOf(t *testing.T, s *Server, id string) string {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, downloadRequest("/jmap/download/"+testAccount+"/"+id+"/m.eml"))
	if w.Code != http.StatusOK {
		t.Fatalf("download %d", w.Code)
	}
	return w.Body.String()
}

// mimeShape is the content types of the tree, depth first.
func mimeShape(t *testing.T, raw string) []string {
	t.Helper()
	e, err := message.Read(strings.NewReader(raw))
	if e == nil {
		t.Fatalf("parse: %v", err)
	}
	var out []string
	var walk func(*message.Entity)
	walk = func(e *message.Entity) {
		ct, _, _ := e.Header.ContentType()
		out = append(out, ct)
		if mr := e.MultipartReader(); mr != nil {
			for {
				p, err := mr.NextPart()
				if err != nil {
					break
				}
				walk(p)
			}
		} else {
			_, _ = io.Copy(io.Discard, e.Body)
		}
	}
	walk(e)
	return out
}

func TestEmailSetCreateBuildsTheTree(t *testing.T) {
	s, _ := uploadServer(t)
	inbox := inboxID(t, s)
	pic := uploadedID(t, upload(t, s, testAccount, strings.NewReader("\x89PNG not really")))
	latin := uploadedID(t, upload(t, s, testAccount, strings.NewReader("caf\xe9")))
	cases := []struct {
		name, body string
		shape      []string
	}{
		{"text", `"textBody":[{"partId":"t"}],"bodyValues":{"t":{"value":"hello"}}`, []string{"text/plain"}},
		{"text and html", `"textBody":[{"partId":"t"}],"htmlBody":[{"partId":"h"}],"bodyValues":{"t":{"value":"hi"},"h":{"value":"<p>hi</p>"}}`,
			[]string{"multipart/alternative", "text/plain", "text/html"}},
		{"html with an inline image", fmt.Sprintf(`"htmlBody":[{"partId":"h"}],"attachments":[{"blobId":%q,"type":"image/png","disposition":"inline","cid":"p1"}],"bodyValues":{"h":{"value":"<img src=cid:p1>"}}`, pic),
			[]string{"multipart/related", "text/html", "image/png"}},
		{"text with a file in another charset", fmt.Sprintf(`"textBody":[{"partId":"t"}],"attachments":[{"blobId":%q,"type":"text/plain","charset":"iso-8859-1","name":"cafe.txt"}],"bodyValues":{"t":{"value":"see"}}`, latin),
			[]string{"multipart/mixed", "text/plain", "text/plain"}},
		{"bodyStructure", `"bodyStructure":{"type":"multipart/alternative","subParts":[{"partId":"t","type":"text/plain"},{"partId":"h","type":"text/html"}]},"bodyValues":{"t":{"value":"a"},"h":{"value":"<b>a</b>"}}`,
			[]string{"multipart/alternative", "text/plain", "text/html"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := createdID(t, createCall(t, s, fmt.Sprintf(`{"mailboxIds":{%q:true},"from":[{"name":"A","email":"a@example.com"}],"subject":"s",%s}`, inbox, tc.body)))
			raw := rawOf(t, s, id)
			if got := mimeShape(t, raw); fmt.Sprint(got) != fmt.Sprint(tc.shape) {
				t.Errorf("tree %v, want %v", got, tc.shape)
			}
			if !strings.Contains(raw, "Message-Id: <") || !strings.Contains(raw, "Date: ") {
				t.Errorf("no Message-Id or Date:\n%s", raw)
			}
		})
	}
}

// An attachment comes back byte for byte: base64 out, decoded on download.
func TestEmailSetCreateKeepsAttachmentBytes(t *testing.T) {
	s, _ := uploadServer(t)
	payload := "\x00\x01binary\xff" + strings.Repeat("z", 200)
	blob := uploadedID(t, upload(t, s, testAccount, strings.NewReader(payload)))
	id := createdID(t, createCall(t, s, fmt.Sprintf(`{"mailboxIds":{%q:true},"attachments":[{"blobId":%q,"type":"application/octet-stream","name":"b.bin"}]}`, inboxID(t, s), blob)))
	got := emailGet(t, s, fmt.Sprintf(`{"accountId":%q,"ids":[%q],"properties":["attachments"]}`, testAccount, id))
	att := firstEmail(t, got)["attachments"].([]any)[0].(map[string]any)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, downloadRequest("/jmap/download/"+testAccount+"/"+att["blobId"].(string)+"/b.bin"))
	if w.Body.String() != payload {
		t.Errorf("attachment came back %d bytes, want %d", w.Body.Len(), len(payload))
	}
}

func TestEmailSetCreateJoinsTheThread(t *testing.T) {
	s, _ := uploadServer(t)
	s.opts.Storage.Threads = threads.NewCache(time.Minute)
	inbox := inboxID(t, s)
	body := `"textBody":[{"partId":"t"}],"bodyValues":{"t":{"value":"x"}}`
	root := createdID(t, createCall(t, s, fmt.Sprintf(`{"mailboxIds":{%q:true},"messageId":["root@x"],"subject":"Plan",%s}`, inbox, body)))
	out := createCall(t, s, fmt.Sprintf(`{"mailboxIds":{%q:true},"messageId":["reply@x"],"inReplyTo":["root@x"],"subject":"Re: Plan",%s}`, inbox, body))
	reply, _ := out["created"].(map[string]any)["c1"].(map[string]any)
	got := emailGet(t, s, fmt.Sprintf(`{"accountId":%q,"ids":[%q],"properties":["threadId"]}`, testAccount, root))
	if reply["threadId"] != firstEmail(t, got)["threadId"] {
		t.Errorf("reply in %v, root in %v", reply["threadId"], firstEmail(t, got)["threadId"])
	}
}

func TestEmailSetCreateRefusals(t *testing.T) {
	s, _ := uploadServer(t)
	inbox := inboxID(t, s)
	text := `"textBody":[{"partId":"t"}],"bodyValues":{"t":{"value":"x"}}`
	cases := []struct {
		name, email, want string
	}{
		{"server-set property", fmt.Sprintf(`{"mailboxIds":{%q:true},"id":"x",%s}`, inbox, text), "invalidProperties"},
		{"no mailbox", fmt.Sprintf(`{%s}`, text), "invalidProperties"},
		{"both body forms", fmt.Sprintf(`{"mailboxIds":{%q:true},"bodyStructure":{"partId":"t"},%s}`, inbox, text), "invalidProperties"},
		{"missing bodyValue", fmt.Sprintf(`{"mailboxIds":{%q:true},"textBody":[{"partId":"nope"}]}`, inbox), "invalidProperties"},
		{"html in textBody", fmt.Sprintf(`{"mailboxIds":{%q:true},"textBody":[{"partId":"t","type":"text/html"}],"bodyValues":{"t":{"value":"x"}}}`, inbox), "invalidProperties"},
		{"missing blob", fmt.Sprintf(`{"mailboxIds":{%q:true},"attachments":[{"blobId":"U%s-1","type":"text/plain"}]}`, inbox, strings.Repeat("a", 64)), "blobNotFound"},
		{"part with both", fmt.Sprintf(`{"mailboxIds":{%q:true},"textBody":[{"partId":"t","blobId":"x"}],"bodyValues":{"t":{"value":"x"}}}`, inbox), "invalidProperties"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := createCall(t, s, tc.email)
			if got := setErrType(out, "notCreated", "c1"); got != tc.want {
				t.Errorf("answered %v, want %s: %v", got, tc.want, out["notCreated"])
			}
		})
	}
}

// Every missing blob is listed in one blobNotFound (RFC 8621 §4.6).
func TestEmailSetCreateNamesEveryMissingBlob(t *testing.T) {
	s, _ := uploadServer(t)
	a, b := "U"+strings.Repeat("a", 64)+"-1", "U"+strings.Repeat("b", 64)+"-2"
	out := createCall(t, s, fmt.Sprintf(`{"mailboxIds":{%q:true},"attachments":[{"blobId":%q,"type":"text/plain"},{"blobId":%q,"type":"text/plain"}]}`, inboxID(t, s), a, b))
	notCreated, _ := out["notCreated"].(map[string]any)
	e, _ := notCreated["c1"].(map[string]any)
	if e["type"] != "blobNotFound" || fmt.Sprint(e["notFound"]) != fmt.Sprint([]any{a, b}) {
		t.Errorf("answered %v, want blobNotFound naming both", e)
	}
}

// header:* properties on create are written in their form (RFC 8621 §4.6).
func TestEmailSetCreateWritesHeaderProperties(t *testing.T) {
	s, _ := uploadServer(t)
	inbox := inboxID(t, s)
	text := `"textBody":[{"partId":"t"}],"bodyValues":{"t":{"value":"x"}}`
	cases := []struct {
		name, props string
		want        []string
		absent      string
	}{
		{"text", `"header:User-Agent:asText":"Mail/0.40.2"`, []string{"User-Agent: Mail/0.40.2\r\n"}, ""},
		{"text not ASCII", `"header:X-Note:asText":"Café"`, []string{"X-Note: =?utf-8?q?Caf=C3=A9?=\r\n"}, ""},
		{"raw folded", `"header:X-Raw":" a\r\n b"`, []string{"X-Raw: a b\r\n"}, ""},
		{"raw without the space", `"header:X-Raw:asRaw":"plain"`, []string{"X-Raw: plain\r\n"}, ""},
		{"addresses", `"header:Resent-To:asAddresses":[{"name":"Ann","email":"ann@example.com"}]`,
			[]string{"Resent-To: \"Ann\" <ann@example.com>\r\n"}, ""},
		{"grouped addresses", `"header:Resent-Cc:asGroupedAddresses":[{"name":"Team","addresses":[{"email":"a@example.com"}]},{"name":null,"addresses":[{"email":"b@example.com"}]}]`,
			[]string{"Resent-Cc: Team: <a@example.com>;, <b@example.com>\r\n"}, ""},
		{"message ids", `"header:Resent-Message-ID:asMessageIds":["r1@example.com"]`, []string{"Resent-Message-Id: <r1@example.com>\r\n"}, ""},
		{"date", `"header:Resent-Date:asDate":"2026-10-24T19:37:10+03:00"`, []string{"Resent-Date: Sat, 24 Oct 2026 19:37:10 +0300\r\n"}, ""},
		{"urls", `"header:List-Unsubscribe:asURLs":["mailto:u@example.com","https://example.com/u"]`,
			[]string{"List-Unsubscribe: <mailto:u@example.com>, <https://example.com/u>\r\n"}, ""},
		{"all, in order", `"header:X-Tag:asText:all":["one","two"]`, []string{"X-Tag: one\r\nX-Tag: two\r\n"}, ""},
		{"null writes nothing", `"header:X-None:asText":null`, nil, "X-None"},
		{"text line break stays encoded", `"header:X-A:asText":"1\nBcc: x@example.com"`, []string{"X-A: =?utf-8?q?1=0ABcc"}, "\r\nBcc:"},
		{"date replaces the generated one", `"header:Date:asDate":"2026-01-02T03:04:05Z"`, []string{"Date: Fri, 02 Jan 2026 03:04:05 +0000\r\n"}, ""},
		{"message id replaces the generated one", `"header:Message-ID:asMessageIds":["given@example.com"]`, []string{"Message-Id: <given@example.com>\r\n"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := createdID(t, createCall(t, s, fmt.Sprintf(`{"mailboxIds":{%q:true},%s,%s}`, inbox, tc.props, text)))
			raw := rawOf(t, s, id)
			head, _, _ := strings.Cut(raw, "\r\n\r\n")
			head += "\r\n"
			for _, w := range tc.want {
				if !strings.Contains(head, w) {
					t.Errorf("no %q in\n%s", w, head)
				}
			}
			if tc.absent != "" && strings.Contains(head, tc.absent) {
				t.Errorf("%q written:\n%s", tc.absent, head)
			}
			for _, f := range []string{"\r\nDate: ", "\r\nMessage-Id: ", "\r\nMime-Version: "} {
				if n := strings.Count("\r\n"+head, f); n != 1 {
					t.Errorf("%d %q fields:\n%s", n, strings.TrimSpace(f), head)
				}
			}
		})
	}
}

// The field a client set comes back through Email/get in the same form.
func TestEmailSetCreateHeaderReadsBack(t *testing.T) {
	s, _ := uploadServer(t)
	id := createdID(t, createCall(t, s, fmt.Sprintf(`{"mailboxIds":{%q:true},"header:User-Agent:asText":"Mail/0.40.2","textBody":[{"partId":"t"}],"bodyValues":{"t":{"value":"x"}}}`, inboxID(t, s))))
	got := emailGet(t, s, fmt.Sprintf(`{"accountId":%q,"ids":[%q],"properties":["header:User-Agent:asText"]}`, testAccount, id))
	if v := firstEmail(t, got)["header:User-Agent:asText"]; v != "Mail/0.40.2" {
		t.Errorf("read back %v", v)
	}
}

// A part's header:* properties land on that part; header:Content-Type gives
// its parameters and the server keeps the charset it encodes in.
func TestEmailSetCreatePartHeaderProperties(t *testing.T) {
	s, _ := uploadServer(t)
	id := createdID(t, createCall(t, s, fmt.Sprintf(`{"mailboxIds":{%q:true},"textBody":[{"partId":"t","header:Content-Type":" text/plain; format=flowed; charset=us-ascii","header:X-Part:asText":"p"}],"bodyValues":{"t":{"value":"x"}}}`, inboxID(t, s))))
	e, err := message.Read(strings.NewReader(rawOf(t, s, id)))
	if e == nil {
		t.Fatalf("parse: %v", err)
	}
	ct, params, _ := e.Header.ContentType()
	if ct != "text/plain" || params["format"] != "flowed" || params["charset"] != "utf-8" {
		t.Errorf("Content-Type %s %v", ct, params)
	}
	if v := e.Header.Get("X-Part"); v != "p" {
		t.Errorf("X-Part %q", v)
	}
}

func TestEmailSetCreateHeaderRefusals(t *testing.T) {
	s, _ := uploadServer(t)
	inbox := inboxID(t, s)
	text := `"textBody":[{"partId":"t"}],"bodyValues":{"t":{"value":"x"}}`
	cases := []struct {
		name, email string
		want        []string
	}{
		{"headers", fmt.Sprintf(`{"mailboxIds":{%q:true},"headers":[],%s}`, inbox, text), []string{"headers"}},
		{"same field as a property", fmt.Sprintf(`{"mailboxIds":{%q:true},"from":[{"email":"a@example.com"}],"header:From:asAddresses":[{"email":"b@example.com"}],%s}`, inbox, text),
			[]string{"from", "header:From:asAddresses"}},
		{"same field twice", fmt.Sprintf(`{"mailboxIds":{%q:true},"header:X-A":" 1","header:x-a:asText":"2",%s}`, inbox, text),
			[]string{"header:X-A", "header:x-a:asText"}},
		{"Content- on the Email", fmt.Sprintf(`{"mailboxIds":{%q:true},"header:Content-Type":" text/html",%s}`, inbox, text), []string{"header:Content-Type"}},
		{"form not allowed for the field", fmt.Sprintf(`{"mailboxIds":{%q:true},"header:Subject:asDate":"2026-01-02T03:04:05Z",%s}`, inbox, text), []string{"header:Subject:asDate"}},
		{"unknown form", fmt.Sprintf(`{"mailboxIds":{%q:true},"header:X-A:asBogus":"1",%s}`, inbox, text), []string{"header:X-A:asBogus"}},
		{"name with a space", fmt.Sprintf(`{"mailboxIds":{%q:true},"header:X A:asText":"1",%s}`, inbox, text), []string{"header:X A:asText"}},
		{"raw with a second field", fmt.Sprintf(`{"mailboxIds":{%q:true},"header:X-A":" 1\r\nBcc: x@example.com",%s}`, inbox, text), []string{"header:X-A"}},
		{"value of the wrong type", fmt.Sprintf(`{"mailboxIds":{%q:true},"header:Resent-To:asAddresses":"a@example.com",%s}`, inbox, text), []string{"header:Resent-To:asAddresses"}},
		{"date not RFC 3339", fmt.Sprintf(`{"mailboxIds":{%q:true},"header:Resent-Date:asDate":"yesterday",%s}`, inbox, text), []string{"header:Resent-Date:asDate"}},
		{"headers on a part", fmt.Sprintf(`{"mailboxIds":{%q:true},"textBody":[{"partId":"t","headers":[]}],"bodyValues":{"t":{"value":"x"}}}`, inbox), []string{"textBody/headers"}},
		{"Content-Transfer-Encoding on a part", fmt.Sprintf(`{"mailboxIds":{%q:true},"textBody":[{"partId":"t","header:Content-Transfer-Encoding":" 8bit"}],"bodyValues":{"t":{"value":"x"}}}`, inbox),
			[]string{"textBody/header:Content-Transfer-Encoding"}},
		{"part type and header:Content-Type", fmt.Sprintf(`{"mailboxIds":{%q:true},"textBody":[{"partId":"t","type":"text/plain","header:Content-Type":" text/plain"}],"bodyValues":{"t":{"value":"x"}}}`, inbox),
			[]string{"textBody/header:Content-Type", "textBody/type"}},
		{"header:Content-Type against htmlBody", fmt.Sprintf(`{"mailboxIds":{%q:true},"htmlBody":[{"partId":"h","header:Content-Type":" text/plain"}],"bodyValues":{"h":{"value":"x"}}}`, inbox),
			[]string{"htmlBody"}},
		{"bodyStructure repeats an Email field", fmt.Sprintf(`{"mailboxIds":{%q:true},"subject":"s","bodyStructure":{"partId":"t","type":"text/plain","header:Subject:asText":"t"},"bodyValues":{"t":{"value":"x"}}}`, inbox),
			[]string{"bodyStructure/header:Subject:asText"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := createCall(t, s, tc.email)
			notCreated, _ := out["notCreated"].(map[string]any)
			e, _ := notCreated["c1"].(map[string]any)
			if e["type"] != "invalidProperties" || fmt.Sprint(e["properties"]) != fmt.Sprint(tc.want) {
				t.Errorf("answered %v, want invalidProperties %v", e, tc.want)
			}
		})
	}
}
