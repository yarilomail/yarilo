package imap_test

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/mailboxmetrics"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// virtualLocks is a real lock service: IDLE and NOTIFY hear another session
// only through its events.
func virtualLocks(t *testing.T) func(*imapserver.Options) {
	t.Helper()
	dir, err := os.MkdirTemp("", "yl-vs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "l.sock")
	backend := locks.NewMemoryBackend(locks.WithSweepInterval(5 * time.Millisecond))
	srv := locks.NewServer(backend, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil)
	ln, err := locks.ListenUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, ln); close(done) }()
	c, err := locks.NewClient(context.Background(), locks.DialUnix(sock))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.Close()
		srv.Close()
		cancel()
		_ = backend.Close()
		<-done
	})
	return func(o *imapserver.Options) { o.Locker = c }
}

func appendInbox(t *testing.T, conn net.Conn, rd *bufio.Reader, tag, subject string) {
	t.Helper()
	raw := "Subject: " + subject + "\r\nFrom: a@test\r\n\r\nbody\r\n"
	fmt.Fprintf(conn, "%s APPEND INBOX {%d+}\r\n%s\r\n", tag, len(raw), raw)
	if line := readTagged(t, rd, tag); !strings.Contains(line, "OK") {
		t.Fatalf("APPEND answered %q", line)
	}
}

func linesWith(lines []string, suffix string) []string {
	var out []string
	for _, l := range lines {
		if strings.Contains(l, suffix) {
			out = append(out, l)
		}
	}
	return out
}

// An open virtual mailbox sees what happens in the folders it draws from on
// the next NOOP, without a new SELECT (virtual-sync.c:2052).
func TestAnOpenVirtualMailboxFollowsItsFolders(t *testing.T) {
	conn, rd := virtualServer(t, map[string]string{"All": "INBOX\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "first", nil)
			saveInto(t, box, ui, "INBOX", 2, "second", nil)
		})
	command(t, conn, rd, "a2", "ENABLE CONDSTORE")
	if got := existsCount(t, conn, rd, "a3", "Virtual/All"); got != 2 {
		t.Fatalf("EXISTS = %d, want 2", got)
	}
	var before uint64
	for _, line := range command(t, conn, rd, "a4", "FETCH 1:* (MODSEQ)") {
		if strings.Contains(line, " FETCH ") {
			before = max(before, modseqOf(t, line))
		}
	}
	other, ord := loginTo(t, lastVirtualAddr)
	existsCount(t, other, ord, "b2", "INBOX")

	// New mail.
	appendInbox(t, other, ord, "b3", "third")
	if got := linesWith(command(t, conn, rd, "a5", "NOOP"), " EXISTS"); len(got) != 1 || got[0] != "* 3 EXISTS" {
		t.Errorf("NOOP after new mail answered %v, want * 3 EXISTS", got)
	}

	// A flag set on the copy.
	command(t, other, ord, "b4", `STORE 1 +FLAGS (\Seen)`)
	got := linesWith(command(t, conn, rd, "a6", "NOOP"), " FETCH ")
	if len(got) != 1 || !strings.HasPrefix(got[0], "* 1 FETCH") || !strings.Contains(got[0], `\Seen`) {
		t.Errorf("NOOP after a flag change answered %v, want * 1 FETCH with \\Seen", got)
	}
	changed := linesWith(command(t, conn, rd, "a7", fmt.Sprintf("FETCH 1:* (FLAGS) (CHANGEDSINCE %d)", before)), " FETCH ")
	var seqs []string
	for _, l := range changed {
		seqs = append(seqs, strings.Fields(l)[1])
	}
	if strings.Join(seqs, ",") != "1,3" {
		t.Errorf("CHANGEDSINCE %d answered %v, want the flagged message and the new one", before, changed)
	}

	// The copy expunged in its folder.
	command(t, other, ord, "b5", `STORE 2 +FLAGS.SILENT (\Deleted)`)
	command(t, other, ord, "b6", "EXPUNGE")
	if got := linesWith(command(t, conn, rd, "a8", "NOOP"), " EXPUNGE"); len(got) != 1 || got[0] != "* 2 EXPUNGE" {
		t.Errorf("NOOP after the copy was expunged answered %v, want * 2 EXPUNGE", got)
	}
}

// A message read in a mailbox of unread mail stops matching, but stays until
// EXPUNGE: vanishing mid-read is what the reference defers (virtual-sync.c:1092).
func TestWhatStopsMatchingStaysUntilExpunge(t *testing.T) {
	conn, rd := virtualServer(t, map[string]string{"Unseen": "INBOX\n  unseen\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "one", nil)
			saveInto(t, box, ui, "INBOX", 2, "two", nil)
		})
	if got := existsCount(t, conn, rd, "a2", "Virtual/Unseen"); got != 2 {
		t.Fatalf("EXISTS = %d, want 2", got)
	}
	command(t, conn, rd, "a3", `STORE 1 +FLAGS (\Seen)`)
	if got := linesWith(command(t, conn, rd, "a4", "NOOP"), " EXPUNGE"); len(got) != 0 {
		t.Errorf("NOOP answered %v, want the read message kept", got)
	}
	if got := linesWith(command(t, conn, rd, "a5", "EXPUNGE"), " EXPUNGE"); len(got) != 1 || got[0] != "* 1 EXPUNGE" {
		t.Errorf("EXPUNGE answered %v, want * 1 EXPUNGE for what stopped matching", got)
	}
	existsCount(t, conn, rd, "a6", "INBOX")
	if f := flagsOf(t, fetchLine(t, conn, rd, "a7", "FETCH 1 (FLAGS)")); strings.Contains(f, `\Deleted`) {
		t.Errorf("the copy was marked for deletion: FLAGS (%s); leaving the view is not deleting", f)
	}
}

// A NOOP on a mailbox whose folders did not move reads no message: no body,
// no index question for a text rule.
func TestANoopOverUnmovedFoldersReadsNothing(t *testing.T) {
	fake := &fakeFTS{setHits: map[string][]uint32{"INBOX": {1}}}
	conn, rd := virtualServerFTS(t, map[string]string{"Needles": "INBOX\n  subject needle\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "needle one", nil)
			saveInto(t, box, ui, "INBOX", 2, "other", nil)
		}, fake)
	if got := existsCount(t, conn, rd, "a2", "Virtual/Needles"); got != 1 {
		t.Fatalf("EXISTS = %d, want 1", got)
	}
	asked := len(fake.asked())
	before := mailboxmetrics.MessageOpens("maildir")
	command(t, conn, rd, "a3", "NOOP")
	command(t, conn, rd, "a4", "NOOP")
	if opened := mailboxmetrics.MessageOpens("maildir") - before; opened != 0 {
		t.Errorf("two NOOPs opened %v bodies, want none", opened)
	}
	if n := len(fake.asked()) - asked; n != 0 {
		t.Errorf("two NOOPs asked the index %d times, want none", n)
	}
}

// waitFor reads until a line containing want arrives, or fails after d.
func waitFor(t *testing.T, conn net.Conn, rd *bufio.Reader, want string, d time.Duration) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(d))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("no %q within %v: %v", want, d, err)
		}
		if strings.Contains(line, want) {
			return strings.TrimRight(line, "\r\n")
		}
	}
}

// IDLE on a virtual mailbox wakes on mail delivered to a folder it draws from
// (virtual-storage.c:693).
func TestIdleOnAVirtualMailboxHearsItsFolders(t *testing.T) {
	conn, rd := virtualServerWith(t, map[string]string{"All": "INBOX\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "first", nil)
		}, nil, virtualLocks(t))
	if got := existsCount(t, conn, rd, "a2", "Virtual/All"); got != 1 {
		t.Fatalf("EXISTS = %d, want 1", got)
	}
	other, ord := loginTo(t, lastVirtualAddr)
	fmt.Fprintf(conn, "a3 IDLE\r\n")
	waitFor(t, conn, rd, "+ ", 2*time.Second)
	appendInbox(t, other, ord, "b2", "second")
	if line := waitFor(t, conn, rd, " EXISTS", 3*time.Second); line != "* 2 EXISTS" {
		t.Errorf("IDLE answered %q, want * 2 EXISTS", line)
	}
	fmt.Fprintf(conn, "DONE\r\n")
	readTagged(t, rd, "a3")
}

// NOTIFY on a virtual mailbox reports its new numbers when a folder it draws
// from changes, though nothing wrote to the virtual mailbox itself.
func TestNotifyOnAVirtualMailboxHearsItsFolders(t *testing.T) {
	conn, rd := virtualServerWith(t, map[string]string{"All": "INBOX\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "first", nil)
		}, nil, virtualLocks(t))
	command(t, conn, rd, "a2", `NOTIFY SET STATUS (mailboxes "Virtual/All" (MessageNew MessageExpunge))`)
	other, ord := loginTo(t, lastVirtualAddr)
	appendInbox(t, other, ord, "b2", "second")
	deadline := time.Now().Add(3 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		status := linesWith(command(t, conn, rd, fmt.Sprintf("n%d", i), "NOOP"), "* STATUS")
		if len(status) > 0 {
			if !strings.Contains(status[0], "Virtual/All") || !strings.Contains(status[0], "MESSAGES 2") {
				t.Errorf("NOTIFY answered %v, want Virtual/All with MESSAGES 2", status)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("NOTIFY reported nothing for Virtual/All after mail reached INBOX")
}
