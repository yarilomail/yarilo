package backendapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/dict"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// A purge over many users counts a store other than mdbox as skipped; a lone
// purge keeps its refusal (#2672).
func TestPurgeSkipsAStoreThatIsNotMdbox(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"
	materialiseHome(t, ts, user)
	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/mdbox/purge", "",
		map[string]any{"user": user, "skip_other_drivers": true})
	var resp mdboxPurgeResponse
	if status != http.StatusOK || json.Unmarshal(body, &resp) != nil || resp.Skipped == "" {
		t.Fatalf("status=%d body=%s, want 200 and skipped", status, body)
	}
	if status, body := doJSON(t, ts, http.MethodPost, "/api/backend/mdbox/purge", "",
		map[string]any{"user": user}); status != http.StatusBadRequest {
		t.Fatalf("lone purge: status=%d body=%s, want the 400 refusal", status, body)
	}
}

// mdboxAltServer serves an mdbox store with an alt tier; writeTimeout, when
// set, is the HTTP server's.
func mdboxAltServer(t *testing.T, writeTimeout time.Duration) (*httptest.Server, *mailbox.Resolver) {
	t.Helper()
	root := t.TempDir()
	d, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n", DefaultAltDir: filepath.Join(root, "alt", "%u")}
	s := New(Options{
		Dicts:        map[string]dict.Dict{"metadata": d},
		Mailbox:      mailbox.Validating(mdbox.New(), mailbox.DefaultNameRules()),
		Index:        file.New(),
		Resolver:     resolver,
		Namespaces:   []config.NamespaceConfig{{Type: "personal", Prefix: "", Separator: "/", List: "yes", Inbox: true}},
		MetadataDict: d,
	})
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.Config.WriteTimeout = writeTimeout
	ts.Start()
	t.Cleanup(ts.Close)
	return ts, resolver
}

// seedDeadRecord saves three messages for user and removes the second.
func seedDeadRecord(t *testing.T, resolver *mailbox.Resolver, user string) {
	t.Helper()
	info, err := resolver.UserInfo(user, "")
	if err != nil {
		t.Fatal(err)
	}
	box := mdbox.New().OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	defer box.Close() //nolint:errcheck
	var names []string
	for _, b := range []string{"Subject: a\r\n\r\na\r\n", "Subject: b\r\n\r\nb\r\n", "Subject: c\r\n\r\nc\r\n"} {
		name, _, _, err := box.Save("INBOX", io.NopCloser(bytes.NewBufferString(b)), 0, int64(len(b)), nil, nil, [16]byte{})
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := box.Remove("INBOX", names[1]); err != nil {
		t.Fatal(err)
	}
}

// altmove purges after the move, as the reference does: a dead record the move
// carried into the rewritten file is reclaimed in the same call (#2672).
func TestAltMoveEndsWithAPurge(t *testing.T) {
	ts, resolver := mdboxAltServer(t, 0)
	const user = "alice@example.com"
	seedDeadRecord(t, resolver, user)

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/mdbox/altmove", "", map[string]any{"user": user})
	var resp mdboxAltMoveResponse
	if status != http.StatusOK || json.Unmarshal(body, &resp) != nil {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if resp.Moved != 2 || resp.Purge == nil || resp.Purge.RecordsExpunged != 1 {
		t.Fatalf("moved %d, purge %+v; want 2 moved and the dead record purged", resp.Moved, resp.Purge)
	}
}

// A purge or an altmove that outlasts the server's write timeout still answers:
// with a timeout of 1 ns every answer is written after it (#2672).
func TestAPurgeOutlastsTheWriteTimeout(t *testing.T) {
	for _, path := range []string{"/api/backend/mdbox/purge", "/api/backend/mdbox/altmove"} {
		t.Run(path, func(t *testing.T) {
			ts, resolver := mdboxAltServer(t, time.Nanosecond)
			const user = "alice@example.com"
			seedDeadRecord(t, resolver, user)
			buf, _ := json.Marshal(map[string]any{"user": user})
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+path, bytes.NewReader(buf))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatalf("no answer: %v", err)
			}
			defer resp.Body.Close() //nolint:errcheck
			body, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"records_expunged":1`)) {
				t.Fatalf("status=%d body=%s err=%v, want 200 with the dead record purged", resp.StatusCode, body, err)
			}
		})
	}
}
