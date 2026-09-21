package dboxv2

import (
	"os"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A delivery is answered only after the body is on the disk, in the mode that
// promises it: a node that loses power in between answered for bytes it does
// not have (#1847, #1969).
func TestTheBodyIsSyncedBeforeASaveAnswers(t *testing.T) {
	tests := []struct {
		name  string
		mode  mailbox.FsyncMode
		syncs int
	}{
		{name: "the default promises the body", mode: "", syncs: 1},
		{name: "optimized promises the body", mode: mailbox.FsyncOptimized, syncs: 1},
		{name: "always promises it too", mode: mailbox.FsyncAlways, syncs: 1},
		{name: "never promises nothing", mode: mailbox.FsyncNever, syncs: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var synced int
			orig := syncFile
			syncFile = func(f *os.File) error {
				synced++
				return orig(f)
			}
			t.Cleanup(func() { syncFile = orig })

			opts := []Option{}
			if tc.mode != "" {
				opts = append(opts, WithFsync(tc.mode))
			}
			box := New(opts...).OpenUser(&mailbox.UserInfo{
				Username: "u1@example.com", Home: t.TempDir(), Driver: "sdbox",
			})
			defer box.Close() //nolint:errcheck
			if err := box.Init(); err != nil {
				t.Fatal(err)
			}
			if err := box.Create("INBOX"); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := box.Save("INBOX", strings.NewReader("From: a@b\r\n\r\nbody\r\n"),
				0, 0, nil, nil, [16]byte{}); err != nil {
				t.Fatalf("save: %v", err)
			}
			if synced != tc.syncs {
				t.Errorf("the body was synced %d times, want %d", synced, tc.syncs)
			}
		})
	}
}

// A sync the volume refuses fails the save: answering anyway is the thing the
// sync is there to prevent.
func TestASaveFailsWhenTheBodySyncFails(t *testing.T) {
	orig := syncFile
	syncFile = func(*os.File) error { return os.ErrClosed }
	t.Cleanup(func() { syncFile = orig })

	home := t.TempDir()
	box := New().OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "sdbox"})
	defer box.Close() //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	name, _, _, err := box.Save("INBOX", strings.NewReader("From: a@b\r\n\r\nbody\r\n"),
		0, 0, nil, nil, [16]byte{})
	if err == nil {
		t.Fatalf("the save answered %q although the body never reached the disk", name)
	}
	msgs, lerr := box.List("INBOX")
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(msgs) != 0 {
		t.Errorf("the folder holds %d messages after a refused sync", len(msgs))
	}
}
