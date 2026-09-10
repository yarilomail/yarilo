package backendapi

import (
	"net/http"
	"testing"
)

// A diagnostic reads: none of these may bring an account into being, and a name
// the layout cannot place is not an account at all (#1774).
func TestAReadCreatesNothingOnDisk(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		body     map[string]any
		wantCode int
	}{
		{"index check", "/api/backend/index/check", map[string]any{"user": "nobody@example.com"}, http.StatusNotFound},
		{"index dump", "/api/backend/index/dump", map[string]any{"user": "nobody@example.com", "folder": "INBOX"}, http.StatusNotFound},
		{"folder list", "/api/backend/folder/list", map[string]any{"user": "nobody@example.com"}, http.StatusNotFound},
		{"folder stats", "/api/backend/folder/stats", map[string]any{"user": "nobody@example.com", "folder": "INBOX"}, http.StatusNotFound},
		{"message get", "/api/backend/message/get", map[string]any{"user": "nobody@example.com", "folder": "INBOX", "uid": 1, "mode": "raw"}, http.StatusNotFound},
		{"user usage", "/api/backend/user/usage", map[string]any{"user": "nobody@example.com"}, http.StatusOK},
		// A bare name: the home template carries %d, so it names no account.
		{"bare name check", "/api/backend/index/check", map[string]any{"user": "nobody"}, http.StatusBadRequest},
		{"bare name dump", "/api/backend/index/dump", map[string]any{"user": "nobody", "folder": "INBOX"}, http.StatusBadRequest},
		{"bare name list", "/api/backend/folder/list", map[string]any{"user": "nobody"}, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts, root := storageTestServer(t)
			before := treeSnapshot(t, root)
			status, body := doJSON(t, ts, http.MethodPost, c.path, "", c.body)
			if status != c.wantCode {
				t.Errorf("status=%d body=%s, want %d", status, body, c.wantCode)
			}
			if after := treeSnapshot(t, root); after != before {
				t.Errorf("the read left something behind:\nbefore: %s\nafter:  %s", before, after)
			}
		})
	}
}

// The name guard is what keeps an eager endpoint from making that tree: a write
// is refused before it opens anything, not after it has made a home.
func TestABareNameIsRefusedByAWriteToo(t *testing.T) {
	ts, root := storageTestServer(t)
	before := treeSnapshot(t, root)
	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/folder/create", "",
		map[string]any{"user": "nobody", "folder": "Work"})
	if status != http.StatusBadRequest {
		t.Errorf("status=%d body=%s, want 400", status, body)
	}
	if after := treeSnapshot(t, root); after != before {
		t.Errorf("a refused write left a tree behind:\nbefore: %s\nafter:  %s", before, after)
	}
}

// And a write still brings it into being, or the rows above would pass on a
// server that refuses everything.
func TestAWriteStillCreatesTheHome(t *testing.T) {
	ts, root := storageTestServer(t)
	before := treeSnapshot(t, root)
	materialiseHome(t, ts, "fresh@example.com")
	if after := treeSnapshot(t, root); after == before {
		t.Error("the tree is unchanged after a folder was created")
	}
	if status, body := doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "",
		map[string]any{"user": "fresh@example.com"}); status != http.StatusOK {
		t.Errorf("folder/list after the write: status=%d body=%s", status, body)
	}
}
