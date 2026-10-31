package backendapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const settleUser = "alice@example.com"

// storedMail is a server whose INBOX holds two messages an MDA left in storage,
// which no session has opened yet.
func storedMail(t *testing.T) *httptest.Server {
	t.Helper()
	ts, root := storageTestServer(t)
	md := filepath.Join(root, "example.com", "alice", "Maildir")
	for _, d := range []string{"cur", "new", "tmp"} {
		if err := os.MkdirAll(filepath.Join(md, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, raw := range map[string]string{
		"1700000000.M1P1.host:2,S": "From: a@example.com\r\nSubject: one\r\n\r\nbody\r\n",
		"1700000001.M2P1.host:2,":  "From: a@example.com\r\nSubject: two\r\n\r\nbody\r\n",
	} {
		if err := os.WriteFile(filepath.Join(md, "cur", name), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return ts
}

// Each verb that reports what a folder holds settles it itself, as a SELECT
// does, so stored mail counts; index dump shows the index as it is (#2391).
func TestReportingReadsSettleTheFolder(t *testing.T) {
	inbox := map[string]any{"user": settleUser, "folder": "INBOX"}
	cases := []struct {
		name   string
		method string
		path   string
		body   any
		want   string // in the answer
	}{
		{"folder info", http.MethodPost, "/api/backend/folder/info", inbox, `"messages":2`},
		{"folder stats", http.MethodPost, "/api/backend/folder/stats", inbox, `"messages":2`},
		{"quota recalc", http.MethodPost, "/api/backend/quota/recalc", map[string]any{"user": settleUser}, `"messages":2`},
		{"quota show", http.MethodGet, "/api/backend/quota/show?user=" + settleUser, nil, `"message_value":2`},
		{"message get", http.MethodPost, "/api/backend/message/get", map[string]any{"user": settleUser, "folder": "INBOX", "uid": 1, "mode": "raw"}, "Subject: one"},
		{"index dump", http.MethodPost, "/api/backend/index/dump", inbox, `"records":[]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := storedMail(t)
			code, out := doJSON(t, ts, tc.method, tc.path, "", tc.body)
			if code != http.StatusOK {
				t.Fatalf("status %d: %s", code, out)
			}
			if got := compact(out); !strings.Contains(got, tc.want) {
				t.Errorf("answer has no %s: %s", tc.want, got)
			}
		})
	}
}

// compact drops the indentation, so a field reads as it does in the asserts.
func compact(b []byte) string {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return string(b)
	}
	out, _ := json.Marshal(v)
	return string(out)
}
