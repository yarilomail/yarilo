package dboxv2

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	fileidx "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The reference names an sdbox message u.<uid> -- its fixtures are u.1 and u.2
// -- and a store of u.<guid> names is one it cannot read (#1704).
func TestASavedMessageIsNamedByItsUID(t *testing.T) {
	_, mb, home := newTestUser(t)
	idx := fileidx.New().OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home})
	defer idx.Close() //nolint:errcheck
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}

	saved, _, guid, err := mb.Save("INBOX", strings.NewReader("msg\n"), 0, 4, nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	namer, ok := mb.(mailbox.UIDNamer)
	if !ok {
		t.Fatal("the sdbox driver does not name a message by its uid")
	}
	appender, ok := idx.(mailbox.NamingAppender)
	if !ok {
		t.Fatal("the index cannot settle a name inside the cycle that allocates the uid")
	}
	m := &mailbox.MessageMeta{Size: 4, VSize: 4, GUID: guid}
	if err := appender.AllocateAndAppendNamed(folder.ID, m, func(uid uint32) (string, error) {
		return namer.AssignUID("INBOX", saved, uid)
	}); err != nil {
		t.Fatal(err)
	}

	want := "u." + strconv.FormatUint(uint64(m.UID), 10)
	if got, err := mailboxbase.MessagePath(mb, "INBOX", m); err != nil || got != want {
		t.Errorf("the driver names it %q (%v), want %q", got, err, want)
	}
	entries, err := os.ReadDir(filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			got = append(got, e.Name())
		}
	}
	if len(got) != 1 || got[0] != want {
		t.Errorf("the folder holds %v, want [%s]", got, want)
	}
}

// saveNamed is the two-step a save now takes: the body first, its name once a
// uid exists. Tests that only need a stored message use it.
func saveNamed(t *testing.T, mb mailbox.UserMailbox, folder, body string, uid uint32, guid [16]byte) (string, uint32) {
	t.Helper()
	name, vsize, _ := saveNamedGUID(t, mb, folder, body, uid, guid)
	return name, vsize
}

// saveNamedGUID is the same two steps, handing back what the save minted.
func saveNamedGUID(t *testing.T, mb mailbox.UserMailbox, folder, body string, uid uint32, guid [16]byte) (string, uint32, [16]byte) {
	t.Helper()
	temp, vsize, g, err := mb.Save(folder, strings.NewReader(body), 0, int64(len(body)), nil, nil, guid)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	named, err := mb.(mailbox.UIDNamer).AssignUID(folder, temp, uid)
	if err != nil {
		t.Fatalf("assign uid %d: %v", uid, err)
	}
	return named, vsize, g
}

// appendRecorder records what an APPEND takes and, on release, what the folder
// then holds: the two together say whether the name was settled inside.
type appendRecorder struct {
	mu     sync.Mutex
	taken  []string
	atFree []string
	dir    string
}

func (l *appendRecorder) Lock(_ context.Context, resource, _ string, _ time.Duration) (locks.Lock, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.taken = append(l.taken, resource)
	return locks.Lock{ID: resource, Resource: resource}, nil
}

func (l *appendRecorder) LockShared(ctx context.Context, r, o string, ttl time.Duration) (locks.Lock, error) {
	return l.Lock(ctx, r, o, ttl)
}

func (l *appendRecorder) Unlock(_ context.Context, _ string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries, _ := os.ReadDir(l.dir)
	l.atFree = nil
	for _, e := range entries {
		l.atFree = append(l.atFree, e.Name())
	}
	return nil
}
func (l *appendRecorder) Renew(context.Context, string, time.Duration) error { return nil }
func (l *appendRecorder) HoldsResource(string) (locks.HoldMode, bool)        { return locks.HoldNone, false }
func (l *appendRecorder) Close() error                                       { return nil }
func (l *appendRecorder) Subscribe(context.Context, string) (<-chan locks.Event, error) {
	return nil, nil
}
func (l *appendRecorder) Emit(context.Context, string, locks.EventType, string) error { return nil }
func (l *appendRecorder) IncrementCounter(context.Context, string, int64) (int64, error) {
	return 0, nil
}

// An APPEND leaves the message wearing the name its uid gives it: a store that
// named it anything else is one the reference cannot read (#1704).
func TestAnAppendNamesTheMessageByItsUID(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails")
	info := &mailbox.UserInfo{Username: "alice@example.com", Home: home}
	mb := New().OpenUser(info)
	if err := mb.Init(); err != nil {
		t.Fatal(err)
	}
	idx := fileidx.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}

	temp, vsize, guid, err := mb.Save("INBOX", strings.NewReader("msg\n"), 0, 4, nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{Size: 4, VSize: vsize, GUID: guid}
	if err := mailboxbase.RecordSaved(idx, mb, folder.ID, "INBOX", temp, m); err != nil {
		t.Fatal(err)
	}

	want := "u." + strconv.FormatUint(uint64(m.UID), 10)
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	found := false
	for _, n := range names {
		if n == want {
			found = true
		}
	}
	if !found {
		t.Errorf("the folder holds %v, and none of it is %s", names, want)
	}
}

// Every path that puts a file in a folder names it u.<uid>: one still minting a
// name from a GUID would leave a store the reference cannot read (#1704).
func TestNoPathWritesAGUIDName(t *testing.T) {
	_, mb, home := newTestUser(t)
	idx := fileidx.New().OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home})
	defer idx.Close() //nolint:errcheck
	if err := mb.Create("Archive"); err != nil {
		t.Fatal(err)
	}
	inbox, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}

	// Saved.
	temp, vsize, guid, err := mb.Save("INBOX", strings.NewReader("msg\n"), 0, 4, nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{Size: 4, VSize: vsize, GUID: guid}
	if err := mailboxbase.RecordSaved(idx, mb, inbox.ID, "INBOX", temp, m); err != nil {
		t.Fatal(err)
	}
	// Copied.
	copier, ok := mb.(interface {
		Copy(srcFolder, srcFilename, dstFolder string, dstUID uint32) (string, error)
	})
	if !ok {
		t.Fatal("the sdbox driver no longer copies")
	}
	srcName, err := mailboxbase.MessagePath(mb, "INBOX", m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := copier.Copy("INBOX", srcName, "Archive", 9); err != nil {
		t.Fatal(err)
	}
	// Moved, into a folder where that very name is taken: the collision path.
	second, _ := saveNamed(t, mb, "INBOX", "other\n", 2, [16]byte{})
	if _, cerr := copier.Copy("INBOX", second, "Archive", 2); cerr != nil {
		t.Fatal(cerr)
	}
	moved, _, err := mb.Move("INBOX", "Archive", second, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	// A collision parks the file, and the destination's uid names it.
	archive, err := idx.OpenFolder("Archive", 1)
	if err != nil {
		t.Fatal(err)
	}
	nm := &mailbox.MessageMeta{Size: 6, VSize: 6}
	if err := mailboxbase.RecordSaved(idx, mb, archive.ID, "Archive", moved, nm); err != nil {
		t.Fatal(err)
	}

	for _, folder := range []string{"INBOX", "Archive"} {
		entries, rerr := os.ReadDir(filepath.Join(home, "sdbox", "mailboxes", folder, "dbox-Mails"))
		if rerr != nil {
			t.Fatal(rerr)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			rest, ok := strings.CutPrefix(e.Name(), "u.")
			if !ok {
				t.Errorf("%s/%s is named by neither uid nor anything the reference reads", folder, e.Name())
				continue
			}
			if _, cerr := strconv.ParseUint(rest, 10, 32); cerr != nil {
				t.Errorf("%s/%s: %q is not a uid", folder, e.Name(), rest)
			}
		}
	}
}

// A save that never got a uid is a crash's leftover once it is old enough; one
// still young is a save in flight and must survive (#1704).
func TestOnlyAnOldTempIsSweptAway(t *testing.T) {
	_, mb, home := newTestUser(t)
	idx := fileidx.New().OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home})
	defer idx.Close() //nolint:errcheck
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails")

	old, _, _, err := mb.Save("INBOX", strings.NewReader("old\n"), 0, 4, nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	long := time.Now().Add(-mailbox.StaleTemp - time.Hour)
	if err := os.Chtimes(filepath.Join(dir, old), long, long); err != nil {
		t.Fatal(err)
	}
	young, _, _, err := mb.Save("INBOX", strings.NewReader("young\n"), 0, 6, nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := mailboxbase.Open(mb, idx).Folder("INBOX", folder.UIDValidity); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, old)); !os.IsNotExist(err) {
		t.Errorf("the old temp survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, young)); err != nil {
		t.Errorf("a save in flight was swept away: %v", err)
	}
}

// A move into a folder where that name is taken ends with the message under the
// destination's own uid, and no parked file left in either folder (#1704).
func TestAMoveIntoATakenNameEndsUnderTheDestinationUID(t *testing.T) {
	_, mb, home := newTestUser(t)
	idx := fileidx.New().OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home})
	defer idx.Close() //nolint:errcheck
	if err := mb.Create("Archive"); err != nil {
		t.Fatal(err)
	}
	archive, err := idx.OpenFolder("Archive", 1)
	if err != nil {
		t.Fatal(err)
	}
	// u.1 exists in both folders: the move collides on the name.
	saveNamed(t, mb, "INBOX", "one\n", 1, [16]byte{})
	saveNamed(t, mb, "Archive", "other\n", 1, [16]byte{})

	moved, _, err := mb.Move("INBOX", "Archive", "u.1", [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{Size: 4, VSize: 4}
	if err := mailboxbase.RecordSaved(idx, mb, archive.ID, "Archive", moved, m); err != nil {
		t.Fatal(err)
	}
	name, err := mailboxbase.MessagePath(mb, "Archive", m)
	if err != nil {
		t.Fatal(err)
	}
	if want := "u." + strconv.FormatUint(uint64(m.UID), 10); name != want {
		t.Errorf("the moved message is %q, want %q", name, want)
	}
	for _, folder := range []string{"INBOX", "Archive"} {
		entries, rerr := os.ReadDir(filepath.Join(home, "sdbox", "mailboxes", folder, "dbox-Mails"))
		if rerr != nil {
			t.Fatal(rerr)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".temp.") {
				t.Errorf("%s still holds a parked file %s", folder, e.Name())
			}
		}
	}
	if rc, ferr := mailboxbase.OpenMessage(mb, "Archive", m); ferr != nil {
		t.Errorf("the moved message cannot be read: %v", ferr)
	} else {
		rc.Close() //nolint:errcheck
	}
}
