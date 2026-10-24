package jmap

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gojmap "git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail/email"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
)

// A real client library uploads (it takes 201 only from the yarilo-patches
// fork) and imports through the session the server advertises.
func TestClientLibraryUploadsAndImports(t *testing.T) {
	s, _ := uploadServer(t)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = trustedPeer
		r.Header.Set(hdrUser, testUser)
		r.Header.Set(hdrSessionID, "deadbeefdeadbeef")
		r.Header.Set(hdrProxyTTL, "4")
		s.Handler().ServeHTTP(w, r)
	}))
	defer front.Close()
	s.opts.Limits.BaseURL = front.URL

	c := &gojmap.Client{SessionEndpoint: front.URL + jmapcore.SessionPath, HttpClient: front.Client()}
	if err := c.Authenticate(); err != nil {
		t.Fatalf("session: %v", err)
	}
	up, err := c.Upload(gojmap.ID(testAccount), strings.NewReader(importable))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	req := &gojmap.Request{}
	req.Invoke(&email.Import{
		Account: gojmap.ID(testAccount),
		Emails: map[string]*email.EmailImport{"c1": {
			BlobID:     up.ID,
			MailboxIDs: map[gojmap.ID]bool{gojmap.ID(inboxID(t, s)): true},
		}},
	})
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	got, ok := resp.Responses[0].Args.(*email.ImportResponse)
	if !ok || got.Created["c1"] == nil || got.Created["c1"].ID == "" {
		t.Fatalf("import answered %#v", resp.Responses[0].Args)
	}
}
