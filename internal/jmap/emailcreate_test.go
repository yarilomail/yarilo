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
