package backendapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// guidRebuildServer stores one message in each of two folders and no store:
// the rebuild is what has to find them.
func guidRebuildServer(t *testing.T) (*httptest.Server, string, mailbox.UserIndex, [][16]byte) {
	t.Helper()
	root := t.TempDir()
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}
	const user = "alice@example.com"
	info := resolver.UserInfo(user, "")

	be := maildir.New()
	box := be.OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	ui := file.New().OpenUser(info)
	var guids [][16]byte
	for _, folder := range []string{"INBOX", "Archive"} {
		if folder != "INBOX" {
			if err := box.Create(folder); err != nil {
				t.Fatalf("create %q: %v", folder, err)
			}
		}
		raw := "Subject: " + folder + "\r\n\r\nbody\r\n"
		name, vsize, guid, err := box.Save(folder, strings.NewReader(raw), 1, int64(len(raw)), nil, nil, [16]byte{})
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		f, err := ui.OpenFolder(folder, 0)
		if err != nil {
			t.Fatalf("open folder: %v", err)
		}
		meta := &mailbox.MessageMeta{UID: 1, Size: uint32(len(raw)), VSize: vsize, GUID: guid}
		if err := mailboxbase.NameSaved(box, folder, name, meta); err != nil {
			t.Fatalf("name: %v", err)
		}
		if err := ui.AppendMessage(f.ID, meta); err != nil {
			t.Fatalf("append: %v", err)
		}
		guids = append(guids, guid)
	}
	_ = ui.Close()
	_ = box.Close()

	s := New(Options{
		Mailbox:  be,
		Index:    file.New(),
		Resolver: resolver,
		Namespaces: []config.NamespaceConfig{
			{Type: "personal", Prefix: "", Separator: "/", List: "yes", Inbox: true},
		},
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, user, file.New().OpenUser(info), guids
}

// The endpoint as yarctl calls it: a row on the handle's type does not reach
// the seam where the capability assertion broke (#1711).
func TestTheRebuildEndpointWritesTheCopies(t *testing.T) {
	ts, user, reader, guids := guidRebuildServer(t)
	code, raw := doJSON(t, ts, http.MethodPost, "/api/backend/index/rebuild-guid-store", "",
		map[string]any{"user": user, "namespace": "personal"})
	if code != http.StatusOK {
		t.Fatalf("the rebuild answered %d: %s", code, raw)
	}
	var out struct {
		Folders int `json:"folders"`
		Copies  int `json:"copies"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v: %s", err, raw)
	}
	if out.Folders != 2 || out.Copies != 2 {
		t.Errorf("the rebuild reports %d folders and %d copies, the fixture has 2 and 2", out.Folders, out.Copies)
	}

	// And the store on disk holds them: the number in the answer is not the
	// proof, the file is.
	res, ok := reader.(mailbox.GUIDResolver)
	if !ok {
		t.Fatal("this index resolves no GUID")
	}
	copies, err := res.GUIDCopies(guids)
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 2 {
		t.Fatalf("the store answers %d copies for the two ids the fixture wrote", len(copies))
	}
	folders := map[[16]byte]bool{}
	for _, c := range copies {
		folders[c.FolderGUID] = true
	}
	if len(folders) != 2 {
		t.Errorf("both copies name the same folder (%d distinct), the fixture put them in two", len(folders))
	}
}
