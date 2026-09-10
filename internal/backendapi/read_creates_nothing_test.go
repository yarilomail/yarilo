package backendapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// freshUser names an account nothing has created: a good name, no home.
const freshUser = "fresh@example.com"

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
		{"acl get", "/api/backend/acl/get", map[string]any{"user": "nobody@example.com", "folder": "INBOX"}, http.StatusNotFound},
		{"acl list", "/api/backend/acl/list", map[string]any{"user": "nobody@example.com", "folder": "INBOX"}, http.StatusNotFound},
		{"metadata get", "/api/backend/metadata/get", map[string]any{"user": "nobody@example.com", "entry": "/private/comment"}, http.StatusNotFound},
		{"metadata list", "/api/backend/metadata/list", map[string]any{"user": "nobody@example.com"}, http.StatusNotFound},
		{"specialuse get", "/api/backend/specialuse/get", map[string]any{"user": "nobody@example.com", "folder": "Sent"}, http.StatusNotFound},
		{"specialuse list", "/api/backend/specialuse/list", map[string]any{"user": "nobody@example.com"}, http.StatusNotFound},
		{"subscriptions list", "/api/backend/subscriptions/list", map[string]any{"user": "nobody@example.com"}, http.StatusNotFound},
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

// A write checks what it was given before it makes anything: a folder name the
// rules refuse leaves no account behind, on a user name that is perfectly good.
func TestARefusedFolderNameMaterialisesNothing(t *testing.T) {
	writes := []struct {
		name string
		path string
		body func(folder string) map[string]any
	}{
		{"acl set", "/api/backend/acl/set", func(f string) map[string]any {
			return map[string]any{"user": freshUser, "folder": f,
				"acl": []map[string]any{{"identifier": "bob@example.com", "rights": "lr"}}}
		}},
		{"folder create", "/api/backend/folder/create", func(f string) map[string]any {
			return map[string]any{"user": freshUser, "folder": f}
		}},
		{"folder delete", "/api/backend/folder/delete", func(f string) map[string]any {
			return map[string]any{"user": freshUser, "folder": f}
		}},
		{"folder expunge", "/api/backend/folder/expunge", func(f string) map[string]any {
			return map[string]any{"user": freshUser, "folder": f}
		}},
		{"folder rename", "/api/backend/folder/rename", func(f string) map[string]any {
			return map[string]any{"user": freshUser, "old_folder": f, "new_folder": "Work"}
		}},
		{"metadata set", "/api/backend/metadata/set", func(f string) map[string]any {
			return map[string]any{"user": freshUser, "folder": f, "entry": "/private/comment", "value": "x"}
		}},
		{"specialuse set", "/api/backend/specialuse/set", func(f string) map[string]any {
			return map[string]any{"user": freshUser, "folder": f, "attr": `\\Sent`}
		}},
		{"subscriptions add", "/api/backend/subscriptions/add", func(f string) map[string]any {
			return map[string]any{"user": freshUser, "folder": f}
		}},
	}
	for _, wr := range writes {
		for _, folder := range []string{".", "/", "..", "../elsewhere", "a/../b"} {
			t.Run(wr.name+" "+folder, func(t *testing.T) {
				ts, root := storageTestServer(t)
				before := treeSnapshot(t, root)
				status, body := doJSON(t, ts, http.MethodPost, wr.path, "", wr.body(folder))
				if status != http.StatusBadRequest {
					t.Errorf("status=%d body=%s, want 400", status, body)
				}
				if after := treeSnapshot(t, root); after != before {
					t.Errorf("a refused name left an account behind:\nbefore: %s\nafter:  %s", before, after)
				}
			})
		}
	}
}

// A write that names no folder has nothing to check and nothing to make: an
// account that is not on disk is answered, not created.
func TestAWriteWithoutAFolderNameMaterialisesNothing(t *testing.T) {
	plain := func(t *testing.T) (*httptest.Server, string, string) {
		ts, root := storageTestServer(t)
		return ts, root, freshUser
	}
	// fts and migrate need a backend that has them wired, or the refusal comes
	// from the missing service and the row never reaches the account.
	withFTS := func(t *testing.T) (*httptest.Server, string, string) {
		ts, root := storageTestServer(t, func(o *Options) { o.FTSClient = &fakeFTS{} })
		return ts, root, freshUser
	}
	withMigrate := func(t *testing.T) (*httptest.Server, string, string) {
		root := t.TempDir()
		return migrateServer(t, root), root, "alice@example.com"
	}
	cases := []struct {
		name     string
		path     string
		setup    func(*testing.T) (*httptest.Server, string, string)
		body     func(user string) map[string]any
		wantCode int
	}{
		{"index rebuild-storage", "/api/backend/index/rebuild-storage", plain,
			func(u string) map[string]any { return map[string]any{"user": u} }, http.StatusNotFound},
		{"index rebuild", "/api/backend/index/rebuild", plain,
			func(u string) map[string]any { return map[string]any{"user": u, "folder": "INBOX"} }, http.StatusNotFound},
		{"index optimize", "/api/backend/index/optimize", plain,
			func(u string) map[string]any { return map[string]any{"user": u, "all": true} }, http.StatusNotFound},
		{"index cache-purge", "/api/backend/index/cache-purge", plain,
			func(u string) map[string]any { return map[string]any{"user": u, "folder": "INBOX"} }, http.StatusNotFound},
		{"quota recalc", "/api/backend/quota/recalc", plain,
			func(u string) map[string]any { return map[string]any{"user": u} }, http.StatusNotFound},
		{"mdbox purge", "/api/backend/mdbox/purge", plain,
			func(u string) map[string]any { return map[string]any{"user": u} }, http.StatusNotFound},
		{"mdbox altmove", "/api/backend/mdbox/altmove", plain,
			func(u string) map[string]any { return map[string]any{"user": u} }, http.StatusNotFound},
		// The user rides in the query string on this one.
		{"fts rescan", "/api/backend/fts/rescan?user=" + freshUser, withFTS,
			func(u string) map[string]any { return nil }, http.StatusNotFound},
		{"subscriptions migrate", "/api/backend/subscriptions/migrate", withMigrate,
			func(u string) map[string]any { return map[string]any{"user": u, "namespace": "user/%u"} }, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts, root, user := c.setup(t)
			before := treeSnapshot(t, root)
			status, body := doJSON(t, ts, http.MethodPost, c.path, "", c.body(user))
			if status != c.wantCode {
				t.Errorf("status=%d body=%s, want %d", status, body, c.wantCode)
			}
			if after := treeSnapshot(t, root); after != before {
				t.Errorf("the write left an account behind:\nbefore: %s\nafter:  %s", before, after)
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
	materialiseHome(t, ts, freshUser)
	if after := treeSnapshot(t, root); after == before {
		t.Error("the tree is unchanged after a folder was created")
	}
	if status, body := doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "",
		map[string]any{"user": "fresh@example.com"}); status != http.StatusOK {
		t.Errorf("folder/list after the write: status=%d body=%s", status, body)
	}
}
