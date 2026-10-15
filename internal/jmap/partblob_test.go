package jmap

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The attachment is base64 on the wire, so only a decoded download matches it.
const partMessage = "From: a@example.com\r\nSubject: parts\r\nMIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
	"--B\r\nContent-Type: text/plain\r\n\r\nhello\r\n" +
	"--B\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=a.bin\r\n" +
	"Content-Transfer-Encoding: base64\r\n\r\naGVsbG8gYXR0YWNobWVudA==\r\n--B--\r\n"

func TestPartBlobIDsDownloadTheirDecodedBody(t *testing.T) {
	s, id := storedServerWithMessage(t, partMessage, 0)
	got := emailGet(t, s, `{"accountId":"`+testUser+`","ids":["`+id+`"],"properties":["attachments","textBody"]}`)
	att := firstEmail(t, got)["attachments"].([]any)[0].(map[string]any)
	blob, _ := att["blobId"].(string)
	if blob != "P"+id+"-2" {
		t.Fatalf("attachment blobId %q, want %q", blob, "P"+id+"-2")
	}
	cases := []struct {
		name, blob string
		code       int
		body       string
	}{
		{"attachment", blob, http.StatusOK, "hello attachment"},
		{"text part", "P" + id + "-1", http.StatusOK, "hello"},
		{"no such part", "P" + id + "-9", http.StatusNotFound, ""},
		{"no such message", "P" + strings.Repeat("0", 32) + "-1", http.StatusNotFound, ""},
		{"part id with a path", "P" + id + "-../1", http.StatusNotFound, ""},
		{"leading zero", "P" + id + "-01", http.StatusNotFound, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, downloadRequest("/jmap/download/"+testUser+"/"+tc.blob+"/x"))
			if w.Code != tc.code {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.code, w.Body)
			}
			if tc.code == http.StatusOK && w.Body.String() != tc.body {
				t.Errorf("body %q, want %q", w.Body.String(), tc.body)
			}
		})
	}
}

// A part of another account's message resolves to nothing (#2208).
func TestPartBlobOfAnotherAccountIsNotFound(t *testing.T) {
	s, foreign := twoAccountServer(t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, downloadRequest("/jmap/download/"+testUser+"/P"+foreign+"-1/x"))
	if w.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", w.Code)
	}
}
