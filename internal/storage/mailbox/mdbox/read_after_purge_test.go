package mdbox

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// purgedUnderSession saves a live and a dropped message, warms the session's
// map with a read, and purges from another handle: m.1 is compacted away.
func purgedUnderSession(t *testing.T) (session *userMailbox, live, dropped, home string) {
	t.Helper()
	home = filepath.Join(t.TempDir(), "home")
	session = openTestUserMailbox(t, home)
	live, _, _, err := session.Save("INBOX", strings.NewReader("live body"), 0, 9, nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	dropped, _, _, err = session.Save("INBOX", strings.NewReader("dropped body"), 0, 12, nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	readBody(t, session, live)
	other := openTestUserMailbox(t, home)
	if err := other.Remove("INBOX", dropped); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Purge(); err != nil {
		t.Fatal(err)
	}
	_ = other.Close()
	if _, err := os.Stat(filepath.Join(home, "mdbox", "storage", "m.1")); !os.IsNotExist(err) {
		t.Fatalf("the purge left m.1 in place: %v", err)
	}
	return session, live, dropped, home
}

func readBody(t *testing.T, u *userMailbox, name string) string {
	t.Helper()
	rc, err := u.Fetch("INBOX", name, false)
	if err != nil {
		t.Fatalf("fetch %s: %v", name, err)
	}
	defer rc.Close() //nolint:errcheck
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A session whose map predates a purge from elsewhere still reads a live
// message: its file moved, and the read follows it (#2100).
func TestASessionReadsAMessageAPurgeMoved(t *testing.T) {
	session, live, _, _ := purgedUnderSession(t)
	if got := readBody(t, session, live); got != "live body" {
		t.Fatalf("read %q after the purge", got)
	}
}

// A record the purge dropped answers expunged: only a zero refcount is
// purged, so no folder holds it, and nothing is marked for a rebuild.
func TestARecordThePurgeDroppedReadsAsExpunged(t *testing.T) {
	session, _, dropped, _ := purgedUnderSession(t)
	_, err := session.Fetch("INBOX", dropped, false)
	if !errors.Is(err, mailbox.ErrExpunged) || errors.Is(err, mailbox.ErrCorruptStorage) {
		t.Fatalf("fetch = %v, want expunged and not corrupt", err)
	}
}

// A file id that did not move and a file still gone is corrupt after exactly
// one reload: the caller's folder index tells an expunge from a loss (#1690).
func TestAFileStillGoneAfterOneReloadIsCorrupt(t *testing.T) {
	session, live, _, home := purgedUnderSession(t)
	readBody(t, session, live)
	if err := os.Remove(filepath.Join(home, "mdbox", "storage", "m.2")); err != nil {
		t.Fatal(err)
	}
	before := testutil.ToFloat64(metricReadRefreshed)
	_, err := session.Fetch("INBOX", live, false)
	if !errors.Is(err, mailbox.ErrCorruptStorage) || errors.Is(err, mailbox.ErrExpunged) {
		t.Fatalf("fetch = %v, want corrupt and not expunged", err)
	}
	if got := testutil.ToFloat64(metricReadRefreshed) - before; got != 1 {
		t.Fatalf("%v reloads for one read, want exactly one", got)
	}
}

// Reads with no purge between them never reload the map.
func TestReadsWithoutAPurgeDoNotReload(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	u := openTestUserMailbox(t, home)
	name, _, _, err := u.Save("INBOX", strings.NewReader("body"), 0, 4, nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	before := testutil.ToFloat64(metricReadRefreshed)
	for i := 0; i < 3; i++ {
		readBody(t, u, name)
	}
	if got := testutil.ToFloat64(metricReadRefreshed) - before; got != 0 {
		t.Fatalf("%v reloads over three plain reads", got)
	}
}
