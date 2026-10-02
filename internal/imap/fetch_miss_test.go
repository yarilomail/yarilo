package imap_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	imap "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

const missMultipart = "From: alice@example.com\r\nSubject: miss probe\r\nMIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"b1\"\r\n\r\n" +
	"--b1\r\nContent-Type: text/plain\r\n\r\none\r\n--b1\r\nContent-Type: text/html\r\n\r\n<p>two</p>\r\n--b1--\r\n"

// messageFile is the one stored message under root.
func messageFile(t *testing.T, root string) string {
	t.Helper()
	var found string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() && (strings.Contains(p, "/cur/") || strings.Contains(p, "/new/")) {
			found = p
		}
		return nil
	})
	if found == "" {
		t.Fatal("message file not found on disk")
	}
	return found
}

// A message read empty is answered NO, and what the empty read parsed to is not
// cached: once the file is whole the same FETCH answers the real item.
func TestAnEmptyReadIsAnsweredNoAndNotCached(t *testing.T) {
	for _, tc := range []struct {
		name, item, want string
		keep             func(whole []byte) []byte
	}{
		{"ENVELOPE", "ENVELOPE", `"miss probe"`, func([]byte) []byte { return nil }},
		{"BODYSTRUCTURE", "BODYSTRUCTURE", `"MIXED"`, func([]byte) []byte { return nil }},
		// Cut before the second part: a structure of the first alone is a lie.
		{"BODYSTRUCTURE of half a file", "BODYSTRUCTURE", `"HTML"`, func(w []byte) []byte { return w[:len(w)/2] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, addr := startEnvelopeCacheServer(t)
			c := dialRaw(t, addr)
			c.login()
			appendRawMessage(t, c, missMultipart)
			c.cmd(`SELECT INBOX`)
			file := messageFile(t, root)
			whole, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, tc.keep(whole), 0o600); err != nil {
				t.Fatal(err)
			}

			out := c.cmd(`FETCH 1 (` + tc.item + `)`)
			if !strings.Contains(out, "NO ") || !strings.Contains(out, "could not be read") {
				t.Errorf("a short read of %s was not answered NO:\n%s", tc.item, out)
			}

			if err := os.WriteFile(file, whole, 0o600); err != nil {
				t.Fatal(err)
			}
			out = c.cmd(`FETCH 1 (` + tc.item + `)`)
			if !strings.Contains(strings.ToUpper(out), strings.ToUpper(tc.want)) {
				t.Errorf("after the file was whole again %s answered what the empty read cached:\n%s", tc.item, out)
			}
		})
	}
}

// opaqueBackend hands out a store that cannot be addressed by record: no
// OpenRecord, and no Unwrap down to the driver that has one.
type opaqueBackend struct{ inner mailbox.MailboxBackend }

type opaqueUser struct{ mailbox.UserMailbox }

func (b opaqueBackend) OpenUser(ui *mailbox.UserInfo) mailbox.UserMailbox {
	return opaqueUser{b.inner.OpenUser(ui)}
}

// An item the store cannot address is counted and logged, not left out of the
// answer in silence.
func TestAnUnaddressableItemIsCounted(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts *imap.FetchOptions
	}{
		{"envelope", &imap.FetchOptions{Envelope: true}},
		{"bodystructure", &imap.FetchOptions{BodyStructure: &imap.FetchItemBodyStructure{Extended: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) { unaddressableRow(t, tc.opts) })
	}
}

func unaddressableRow(t *testing.T, opts *imap.FetchOptions) {
	root := t.TempDir()
	c := startServerWithRoot(t, opaqueBackend{maildirBackend(t)}, root)
	app := c.Append("INBOX", int64(len(missMultipart)), nil)
	if _, err := app.Write([]byte(missMultipart)); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	before := unreadableCount(t, "fetch")
	msgs, err := c.Fetch(imap.SeqSetNum(1), opts).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Envelope != nil || msgs[0].BodyStructure != nil {
		t.Fatalf("the store answered the item after all, so this proves nothing: %+v", msgs)
	}
	if got := unreadableCount(t, "fetch") - before; got != 1 {
		t.Errorf("counted %v for an item the store could not address, want 1", got)
	}
}
