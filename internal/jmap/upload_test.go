package jmap

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailops"
	"github.com/yarilomail/yarilo/pkg/quota"
)

const importable = "From: a@example.com\r\nSubject: imported\r\n\r\nimported body\r\n"

func uploadServer(t *testing.T) (*Server, string) {
	t.Helper()
	s, _, home := storedServerWithMessageAt(t, "Subject: seed\r\n\r\nseed\r\n", 0)
	s.opts.Limits.MaxSizeUpload = 256
	s.opts.UploadTTL = time.Hour
	return s, home
}

func upload(t *testing.T, s *Server, account string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	path := "/jmap/upload/" + account + "/"
	if account == "" {
		path = "/jmap/upload/"
	}
	r := httptest.NewRequest(http.MethodPost, path, body)
	r.RemoteAddr = trustedPeer
	r.Header.Set(hdrUser, testUser)
	r.Header.Set(hdrSessionID, "deadbeefdeadbeef")
	r.Header.Set(hdrProxyTTL, "4")
	r.Header.Set("Content-Type", "message/rfc822")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func uploadedID(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	if w.Code != http.StatusCreated {
		t.Fatalf("upload status %d: %s", w.Code, w.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got["blobId"].(string)
}

func TestUploadStoresByContentInTheControlRoot(t *testing.T) {
	s, home := uploadServer(t)
	first := uploadedID(t, upload(t, s, testUser, strings.NewReader(importable)))
	if !strings.HasPrefix(first, "U") || !strings.HasSuffix(first, fmt.Sprintf("-%d", len(importable))) {
		t.Errorf("blob id %q", first)
	}
	if again := uploadedID(t, upload(t, s, testUser, strings.NewReader(importable))); again != first {
		t.Errorf("the same bytes got %q, then %q", first, again)
	}
	entries, _ := os.ReadDir(filepath.Join(home, "jmap-blobs", "tmp"))
	if len(entries) != 1 || entries[0].Name() != first {
		t.Errorf("store holds %v, want only %s", entries, first)
	}
}

func TestUploadRefusals(t *testing.T) {
	s, _ := uploadServer(t)
	cases := []struct {
		name, account string
		body          io.Reader
		code          int
	}{
		{"another account", foreignUser, strings.NewReader(importable), http.StatusNotFound},
		{"no account", "", strings.NewReader(importable), http.StatusNotFound},
		{"over the cap", testUser, strings.NewReader(strings.Repeat("x", 300)), http.StatusRequestEntityTooLarge},
		// No Content-Length: the body is cut at the cap while it is read.
		{"over the cap, chunked", testUser, io.MultiReader(strings.NewReader(strings.Repeat("x", 300))), http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if w := upload(t, s, tc.account, tc.body); w.Code != tc.code {
				t.Errorf("status %d, want %d: %s", w.Code, tc.code, w.Body)
			}
		})
	}
}

// Live uploads count against the storage limit, so uploads alone cannot fill
// an account past it.
func TestUploadIsHeldToTheQuota(t *testing.T) {
	s, _ := uploadServer(t)
	info, _ := s.opts.Storage.ResolveUser(testUser)
	info.QuotaRules = []string{"*:storage=1K"}
	s.opts.Storage.Ops = mailops.Config{Quota: mailops.QuotaConfig{Engine: true,
		Policy: quota.Policy{StoragePercentage: 100, MessagePercentage: 100}}}
	body := strings.Repeat("y", 200)
	accepted := 0
	for i := 0; i < 8; i++ {
		w := upload(t, s, testUser, strings.NewReader(body+fmt.Sprint(i)))
		if w.Code == http.StatusInsufficientStorage {
			break
		}
		uploadedID(t, w)
		accepted++
	}
	if accepted == 0 || accepted >= 8 {
		t.Errorf("%d uploads of 200 bytes accepted under a 1K limit", accepted)
	}
}

func importCall(t *testing.T, s *Server, blob string, keywords string) map[string]any {
	t.Helper()
	args := fmt.Sprintf(`{"accountId":%q,"emails":{"c1":{"blobId":%q,"mailboxIds":{%q:true},"keywords":%s}}}`,
		testUser, blob, inboxID(t, s), keywords)
	name, out := callAs(t, s, testUser, "Email/import", args)
	if name != "Email/import" {
		t.Fatalf("answered %s %v", name, out)
	}
	return out
}

func TestImportTakesAnUploadIntoAMailbox(t *testing.T) {
	s, _ := uploadServer(t)
	blob := uploadedID(t, upload(t, s, testUser, strings.NewReader(importable)))
	out := importCall(t, s, blob, `{"$seen":true,"work":true}`)
	created, _ := out["created"].(map[string]any)
	c1, _ := created["c1"].(map[string]any)
	id, _ := c1["id"].(string)
	if id == "" || out["newState"] == out["oldState"] {
		t.Fatalf("import %v", out)
	}
	got := emailGet(t, s, `{"accountId":"`+testUser+`","ids":["`+id+`"],"properties":["subject","keywords","mailboxIds"]}`)
	email := firstEmail(t, got)
	kw := fmt.Sprint(email["keywords"])
	if email["subject"] != "imported" || !strings.Contains(kw, "$seen") || !strings.Contains(kw, "work") {
		t.Errorf("imported email %v", email)
	}
}

func TestImportRefusesWhatItCannotRead(t *testing.T) {
	s, home := uploadServer(t)
	// A readable message under an id whose size it does not have.
	forged := "U" + strings.Repeat("b", 64) + "-5"
	if err := os.MkdirAll(filepath.Join(home, "jmap-blobs", "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "jmap-blobs", "tmp", forged), []byte(importable), 0o600); err != nil {
		t.Fatal(err)
	}
	expired := uploadedID(t, upload(t, s, testUser, strings.NewReader(importable+"x")))
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(home, "jmap-blobs", "tmp", expired), old, old); err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]string{
		"unknown upload":    "U" + strings.Repeat("a", 64) + "-5",
		"size not as named": forged,
		"expired":           expired,
		"not an id":         "../../etc/passwd",
	} {
		t.Run(name, func(t *testing.T) {
			notCreated, _ := importCall(t, s, id, `{}`)["notCreated"].(map[string]any)
			setErr, _ := notCreated["c1"].(map[string]any)
			if setErr["type"] != "invalidProperties" || !strings.Contains(fmt.Sprint(setErr["properties"]), "blobId") {
				t.Errorf("answered %v, want invalidProperties blobId", notCreated)
			}
		})
	}
}
