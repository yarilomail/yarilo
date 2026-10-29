package dboxv2

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// A message file damaged on disk reads as corrupt storage, never as a shorter
// or different message: the caller rebuilds or reports instead of serving it.
func TestADamagedMessageFileIsCorruption(t *testing.T) {
	for name, damage := range map[string]func(b []byte) []byte{
		"file header unparsable": func(b []byte) []byte {
			return append([]byte("garbage\n"), b[bytes.IndexByte(b, '\n')+1:]...)
		},
		"message header magic": func(b []byte) []byte { b[bytes.IndexByte(b, '\n')+1] ^= 0xff; return b },
		"message header cut":   func(b []byte) []byte { return b[:bytes.IndexByte(b, '\n')+5] },
		"body cut":             func(b []byte) []byte { return b[:len(b)-60] },
	} {
		t.Run(name, func(t *testing.T) {
			_, mb, _ := newTestUser(t)
			fname, _ := saveNamed(t, mb, "INBOX", "Subject: d\r\n\r\n"+strings.Repeat("body line\r\n", 10), 1, [16]byte{})
			path := filepath.Join(mb.(*userMailbox).folderPath("INBOX"), fname)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, damage(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			rc, err := mb.Fetch("INBOX", fname, false)
			if err == nil {
				_, err = io.ReadAll(rc)
				_ = rc.Close()
			}
			if !errors.Is(err, mailbox.ErrCorruptStorage) {
				t.Errorf("fetch: %v, want corruption", err)
			}
		})
	}
}

// A store whose UIDVALIDITY sits under the reference's file name keeps that
// value: a new one would make every client refetch the whole account.
func TestAUIDValidityUnderTheReferenceNameIsKept(t *testing.T) {
	home := t.TempDir()
	control := filepath.Join(home, "sdbox", "control")
	if err := os.MkdirAll(control, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(control, legacyUIDValidity), []byte("5f00aa11\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mb := New().OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home})
	if err := mb.Init(); err != nil {
		t.Fatal(err)
	}
	v, err := mb.(*userMailbox).UIDValidity()
	if err != nil {
		t.Fatal(err)
	}
	if v != 0x5f00aa11 {
		t.Errorf("uidvalidity %x, want the store's 5f00aa11", v)
	}
}

// A UIDVALIDITY file that does not parse is an error, not a fresh value.
func TestAnUnreadableUIDValidityIsAnError(t *testing.T) {
	_, mb, _ := newTestUser(t)
	u := mb.(*userMailbox)
	if err := os.WriteFile(u.uidValidityPath(), []byte("not hex\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := u.UIDValidity(); err == nil {
		t.Errorf("read uidvalidity %x from garbage", v)
	}
}

// Uid 0 names no message: a copy or a naming under it is refused, and a folder
// that is not there scans and lists as empty.
func TestUIDZeroIsRefusedAndAMissingFolderIsEmpty(t *testing.T) {
	_, mb, _ := newTestUser(t)
	u := mb.(*userMailbox)
	name, _ := saveNamed(t, mb, "INBOX", "Subject: z\r\n\r\nz\r\n", 1, [16]byte{})
	if _, err := u.Copy("INBOX", name, "Archive", 0); err == nil {
		t.Error("a copy under uid 0 was made")
	}
	if _, err := u.AssignUID("INBOX", name, 0); err == nil {
		t.Error("a name under uid 0 was given")
	}
	if recs, err := u.Scan("Nowhere"); err != nil || len(recs) != 0 {
		t.Errorf("scan of a missing folder: %d records, %v", len(recs), err)
	}
	if msgs, err := u.List("Nowhere"); err != nil || len(msgs) != 0 {
		t.Errorf("list of a missing folder: %d messages, %v", len(msgs), err)
	}
}
