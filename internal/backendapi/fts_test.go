package backendapi

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// fakeFTS records calls and scripts Status for the backend-api tests.
type fakeFTS struct {
	mu        sync.Mutex
	status    uint32
	rescans   []string
	wholeUser int
	optimize  int
	expunges  []ftsExpungeCall
}

type ftsExpungeCall struct {
	Folder string
	UID    uint32
	// GUID is what the retraction named: the index retracts by the message,
	// so an empty one is a caller that lost it (#1986).
	GUID [16]byte
}

func (f *fakeFTS) Index(string, fts.MailboxRef, uint32, int) error { return nil }
func (f *fakeFTS) Prepend(string, fts.MailboxRef, uint32) error    { return nil }
func (f *fakeFTS) Expunge(_ string, m fts.MailboxRef, uid uint32, guid [16]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expunges = append(f.expunges, ftsExpungeCall{Folder: m.Name, UID: uid, GUID: guid})
	return nil
}
func (f *fakeFTS) Lookup(string, fts.MailboxRef, fts.Query) (fts.Result, error) {
	return fts.Result{}, nil
}
func (f *fakeFTS) Status(_ string, _ fts.MailboxRef) (uint32, uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, 7, nil
}
func (f *fakeFTS) RescanUser(_ string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wholeUser++
	return []string{"INBOX", "Archive"}, nil
}

func (f *fakeFTS) Rescan(_ string, m fts.MailboxRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rescans = append(f.rescans, m.Name)
	return nil
}
func (f *fakeFTS) Optimize(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.optimize++
	return nil
}
func (f *fakeFTS) Close() error { return nil }

// TestFtsExpungeInvalidatesDroppedUIDs: the session-less operator FTS-notify
// helper invalidates one document per dropped UID, keyed to the folder — the
// path the operator index rebuild uses so dropped records do not linger as ghost
// documents until the next rescan.
func TestFtsExpungeInvalidatesDroppedUIDs(t *testing.T) {
	root := t.TempDir()
	d, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	fake := &fakeFTS{}
	s := New(Options{
		Dicts:    map[string]dict.Dict{"metadata": d},
		Mailbox:  maildir.New(),
		Index:    file.New(),
		Resolver: &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"},
		Namespaces: []config.NamespaceConfig{
			{Type: "personal", Prefix: "", Separator: "/", List: "yes", Inbox: true},
		},
		MetadataDict: d,
		FTSClient:    fake,
	})

	uc, err := s.openUserContext("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()

	s.ftsExpunge(uc, "INBOX", []mailbox.ExpungedCopy{
		{UID: 5, GUID: [16]byte{5}}, {UID: 9, GUID: [16]byte{9}},
	})

	if len(fake.expunges) != 2 {
		t.Fatalf("expunge calls = %d, want 2", len(fake.expunges))
	}
	for _, c := range fake.expunges {
		if c.Folder != "INBOX" {
			t.Errorf("expunge folder = %q, want INBOX", c.Folder)
		}
		// The rebuild's retraction names the message, not just the uid: the
		// index retracts by identity (#1986).
		if c.GUID == ([16]byte{}) {
			t.Errorf("uid %d was retracted without naming the message", c.UID)
		}
	}
	if fake.expunges[0].UID != 5 || fake.expunges[1].UID != 9 {
		t.Errorf("expunged UIDs = %v, want [5 9]", fake.expunges)
	}

	// No FTS client → no-op, no panic.
	s.opts.FTSClient = nil
	s.ftsExpunge(uc, "INBOX", []mailbox.ExpungedCopy{{UID: 1, GUID: [16]byte{1}}})
}

func ftsTestServer(t *testing.T) (*httptest.Server, *fakeFTS, string) {
	t.Helper()
	root := t.TempDir()
	d, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	fake := &fakeFTS{status: 42}
	s := New(Options{
		Dicts:    map[string]dict.Dict{"metadata": d},
		Mailbox:  maildir.New(),
		Index:    file.New(),
		Resolver: &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"},
		Namespaces: []config.NamespaceConfig{
			{Type: "personal", Prefix: "", Separator: "/", List: "yes", Inbox: true},
		},
		MetadataDict: d,
		FTSClient:    fake,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, fake, root
}

func TestFTSStatusEndpoint(t *testing.T) {
	ts, _, root := ftsTestServer(t)
	uc, err := newAdminUserContext(t, ts, root, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	defer uc.cleanup()

	status, body := doJSON(t, ts, http.MethodGet,
		"/api/backend/fts/status?user=alice@example.com&folder=INBOX", "", nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var r struct {
		LastIndexedUID   uint32 `json:"last_indexed_uid"`
		SettingsChecksum uint32 `json:"settings_checksum"`
	}
	decodeJSONBody(t, body, &r)
	if r.LastIndexedUID != 42 || r.SettingsChecksum != 7 {
		t.Fatalf("status = %+v, want {42, 7}", r)
	}
}

func TestFTSRescanAllFolders(t *testing.T) {
	ts, fake, root := ftsTestServer(t)
	uc, err := newAdminUserContext(t, ts, root, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	defer uc.cleanup()
	if err := uc.box.Create("Archive"); err != nil {
		t.Fatal(err)
	}

	status, body := doJSON(t, ts, http.MethodPost,
		"/api/backend/fts/rescan?user=alice@example.com", "", nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var r struct {
		Folders []string `json:"folders"`
	}
	decodeJSONBody(t, body, &r)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	// One call for the user, and the folders are the service's answer: the
	// walk lives where the index is held, not here (#1986).
	if fake.wholeUser != 1 {
		t.Errorf("whole-user rescans = %d, want 1", fake.wholeUser)
	}
	if len(fake.rescans) != 0 {
		t.Errorf("the handler still rescanned folder by folder: %v", fake.rescans)
	}
	if len(r.Folders) != 2 {
		t.Errorf("rescanned folders = %v, want what the service reported", r.Folders)
	}
}

func TestFTSRescanSingleFolder(t *testing.T) {
	ts, fake, root := ftsTestServer(t)
	uc, err := newAdminUserContext(t, ts, root, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	defer uc.cleanup()

	status, _ := doJSON(t, ts, http.MethodPost,
		"/api/backend/fts/rescan?user=alice@example.com&folder=INBOX", "", nil)
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.rescans) != 1 || fake.rescans[0] != "INBOX" {
		t.Fatalf("rescans = %v, want [INBOX]", fake.rescans)
	}
}

func TestFTSOptimizeEndpoint(t *testing.T) {
	ts, fake, _ := ftsTestServer(t)
	status, _ := doJSON(t, ts, http.MethodPost,
		"/api/backend/fts/optimize?user=alice@example.com", "", nil)
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.optimize != 1 {
		t.Fatalf("optimize calls = %d, want 1", fake.optimize)
	}
}

func TestFTSDisabledReturns501(t *testing.T) {
	root := t.TempDir()
	d, _ := dict.Open(dict.Config{Driver: "memory"})
	t.Cleanup(func() { _ = d.Close() })
	s := New(Options{
		Dicts:        map[string]dict.Dict{"metadata": d},
		Mailbox:      maildir.New(),
		Index:        file.New(),
		Resolver:     &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"},
		MetadataDict: d,
		// FTSClient deliberately nil.
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	status, _ := doJSON(t, ts, http.MethodPost,
		"/api/backend/fts/optimize?user=alice@example.com", "", nil)
	if status != http.StatusNotImplemented {
		t.Fatalf("status=%d, want 501", status)
	}
}
