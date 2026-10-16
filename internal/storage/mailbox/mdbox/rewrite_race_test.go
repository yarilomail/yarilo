package mdbox

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A save from a peer session while purge or altmove rewrites the append file
// must survive the source's unlink (#2234).
func TestASaveDuringARewriteSurvivesTheUnlink(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, u *userMailbox)
		rewrite func(u *userMailbox) error
	}{
		{"purge", func(t *testing.T, u *userMailbox) {
			gone := testSaveMsg(t, u, "Subject: gone\r\n\r\ngone\r\n")
			testSaveMsg(t, u, "Subject: kept\r\n\r\nkept\r\n")
			if err := u.Remove("INBOX", gone); err != nil {
				t.Fatal(err)
			}
		}, func(u *userMailbox) error { _, err := u.Purge(); return err }},
		{"altmove", func(t *testing.T, u *userMailbox) {
			testSaveMsg(t, u, "Subject: old\r\n\r\nold\r\n")
		}, func(u *userMailbox) error {
			_, err := u.AltMove(AltMoveQuery{Before: time.Now().Add(time.Second)})
			return err
		}},
	} {
		for _, lk := range []struct {
			name string
			opts func() []Option
		}{
			{"lock service", func() []Option { return []Option{WithLocker(&mutexLocker{})} }},
			{"no lock service", func() []Option { return nil }},
		} {
			t.Run(tc.name+"/"+lk.name, func(t *testing.T) { rewriteRace(t, lk.opts(), tc.prepare, tc.rewrite) })
		}
	}
}

func rewriteRace(t *testing.T, opts []Option, prepare func(*testing.T, *userMailbox), rewrite func(*userMailbox) error) {
	base := t.TempDir()
	info := &mailbox.UserInfo{Username: "alice@example.com", Home: filepath.Join(base, "home"), AltDir: filepath.Join(base, "alt")}
	b := New(opts...)
	open := func() *userMailbox {
		u := b.OpenUser(info).(*userMailbox)
		if err := u.Init(); err != nil {
			t.Fatal(err)
		}
		return u
	}
	u, peer := open(), open()
	prepare(t, u)

	const body = "Subject: racer\r\n\r\nracer\r\n"
	var name string
	done := make(chan error, 1)
	afterRecordsRead = func() {
		afterRecordsRead = nil
		go func() {
			var err error
			name, _, _, err = peer.Save("INBOX", strings.NewReader(body), 0, 0, nil, nil, [16]byte{})
			done <- err
		}()
		// Under the hold the save waits for the rewrite; without it, it
		// lands in the file the rewrite read and is about to unlink.
		select {
		case err := <-done:
			done <- err
		case <-time.After(300 * time.Millisecond):
		}
	}
	t.Cleanup(func() { afterRecordsRead = nil })

	if err := rewrite(u); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("peer save: %v", err)
	}
	if got := testFetchBody(t, peer, name); got != body {
		t.Fatalf("racing save reads %q", got)
	}
}
