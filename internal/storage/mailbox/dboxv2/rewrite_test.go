package dboxv2

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agelib "filippo.io/age"

	"github.com/yarilomail/yarilo/v2/internal/mailcrypt"
	"github.com/yarilomail/yarilo/v2/internal/mailcrypt/refstream"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/dboxref"
	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/locks/lockstest"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// sealRewrite seals the old body's bytes as they lie, and passes Verify.
func sealRewrite(l *mailcrypt.Layer) mailbox.BodyRewrite {
	return mailbox.BodyRewrite{
		Fill: func(src mailbox.RawBody, dst io.Writer) (int64, bool, error) {
			o, err := l.Open(src, nil)
			if err != nil {
				return 0, false, err
			}
			defer o.Close() //nolint:errcheck
			var plain bytes.Buffer
			if _, err := io.Copy(&plain, o); err != nil {
				return 0, false, err
			}
			n := int64(plain.Len())
			return n, true, l.Seal(dst, &plain)
		},
		Verify: func(mailbox.RawBody) error { return nil },
	}
}

func rewriteLayer(t *testing.T) *mailcrypt.Layer {
	t.Helper()
	id, err := agelib.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return mailcrypt.New(mailcrypt.Options{Recipients: []agelib.Recipient{id.Recipient()}, Identities: []agelib.Identity{id}})
}

// rewriteUser is alice's sdbox store against a lock service, as backend-api
// opens it: a rewrite refuses to run without one.
func rewriteUser(t *testing.T) (mailbox.UserMailbox, *userMailbox) {
	t.Helper()
	mb := lockedUser(t, t.TempDir(), lockstest.Server(t)())
	return mb, mb.(*userMailbox)
}

func metaOf(t *testing.T, path string) map[byte]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck
	st, _ := f.Stat()
	_, _, entries, err := readRecordParts(f, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	out := map[byte]string{}
	for _, e := range entries {
		out[e.Key] = e.Value
	}
	return out
}

func noTemps(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), rewritePrefix) {
			t.Fatalf("a rewrite temp was left: %s", e.Name())
		}
	}
}

// The record keeps its name, date, GUID, V and every other entry; the body is
// sealed, Z holds the plaintext's size, and the plaintext is the same bytes.
func TestRewriteKeepsTheRecord(t *testing.T) {
	l := rewriteLayer(t)
	for _, c := range []struct {
		name  string
		write func(t *testing.T, mb mailbox.UserMailbox, path string) string
	}{
		{"saved here", func(t *testing.T, mb mailbox.UserMailbox, _ string) string {
			name, _ := saveNamed(t, mb, "INBOX", "Subject: s\r\n\r\nbody\r\n", 1, [16]byte{})
			return name
		}},
		{"reference file with bare LF", func(t *testing.T, _ mailbox.UserMailbox, path string) string {
			if err := os.WriteFile(path, dboxref.SdboxShort(t), 0o600); err != nil {
				t.Fatal(err)
			}
			return "u.1"
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			mb, u := rewriteUser(t)
			path := filepath.Join(u.folderPath("INBOX"), "u.1")
			name := c.write(t, mb, path)
			m := &mailbox.MessageMeta{UID: 1}
			old := metaOf(t, path)
			oldRaw := rawBytes(t, u, m)
			before, _ := os.Stat(path)

			if err := u.RewriteRecord("INBOX", m, sealRewrite(l)); err != nil {
				t.Fatal(err)
			}
			if name != "u.1" {
				t.Fatalf("name %s", name)
			}
			now := metaOf(t, path)
			for _, k := range []byte{metaKeyGUID, metaKeyReceived, metaKeyVirtualSize, metaKeyOrigMailbox} {
				if old[k] != now[k] {
					t.Errorf("entry %c: %q became %q", k, old[k], now[k])
				}
			}
			if now[metaKeyPhysicalSize] == "" {
				t.Error("no Z on a sealed body")
			}
			after, _ := os.Stat(path)
			if !after.ModTime().Equal(before.ModTime()) {
				t.Errorf("mtime %v became %v", before.ModTime(), after.ModTime())
			}
			raw, err := u.RawRecord("INBOX", m)
			if err != nil {
				t.Fatal(err)
			}
			o, err := l.Open(raw, raw)
			if err != nil {
				t.Fatal(err)
			}
			plain, _ := io.ReadAll(o)
			_ = o.Close()
			if !bytes.Equal(plain, oldRaw) {
				t.Fatalf("plaintext %q, want the old bytes %q", plain, oldRaw)
			}
			noTemps(t, u.folderPath("INBOX"))
		})
	}
}

func rawBytes(t *testing.T, u *userMailbox, m *mailbox.MessageMeta) []byte {
	t.Helper()
	raw, err := u.RawRecord("INBOX", m)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close() //nolint:errcheck
	b, err := io.ReadAll(io.NewSectionReader(raw, 0, raw.Size()))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Whatever goes wrong before the rename, the file is the old one, byte for
// byte, and no temp stays behind.
func TestRewriteLeavesTheFileWhenItCannotFinish(t *testing.T) {
	l := rewriteLayer(t)
	failVerify := sealRewrite(l)
	failVerify.Verify = func(mailbox.RawBody) error { return errors.New("hash differs") }
	cases := []struct {
		name  string
		rw    mailbox.BodyRewrite
		seam  func(path string)
		wantE error
	}{
		{"verification fails", failVerify, nil, nil},
		{"file changed after the read", sealRewrite(l), func(path string) {
			later := time.Now().Add(time.Hour)
			_ = os.Chtimes(path, later, later)
		}, mailbox.ErrRecordChanged},
		{"file expunged after the read", sealRewrite(l), func(path string) { _ = os.Remove(path) }, mailbox.ErrExpunged},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mb, u := rewriteUser(t)
			saveNamed(t, mb, "INBOX", "Subject: s\r\n\r\nbody\r\n", 1, [16]byte{})
			path := filepath.Join(u.folderPath("INBOX"), "u.1")
			if c.seam != nil {
				testAfterRewriteVerified = func() { c.seam(path) }
				t.Cleanup(func() { testAfterRewriteVerified = nil })
			}
			before, _ := os.ReadFile(path)
			err := u.RewriteRecord("INBOX", &mailbox.MessageMeta{UID: 1}, c.rw)
			if err == nil {
				t.Fatal("the rewrite reported success")
			}
			if c.wantE != nil && !errors.Is(err, c.wantE) {
				t.Fatalf("err %v, want %v", err, c.wantE)
			}
			if after, rerr := os.ReadFile(path); rerr == nil && c.wantE == nil && !bytes.Equal(before, after) {
				t.Fatal("the file changed")
			}
			noTemps(t, u.folderPath("INBOX"))
		})
	}
}

// A reader that opened the file before the swap reads the old body to its end.
func TestRewriteLetsAnOpenReaderFinish(t *testing.T) {
	l := rewriteLayer(t)
	mb, u := rewriteUser(t)
	body := "Subject: s\r\n\r\n" + strings.Repeat("line\r\n", 20000)
	name, _ := saveNamed(t, mb, "INBOX", body, 1, [16]byte{})
	rc, err := mb.Fetch("INBOX", name, false)
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 10)
	if _, err := io.ReadFull(rc, head); err != nil {
		t.Fatal(err)
	}
	if err := u.RewriteRecord("INBOX", &mailbox.MessageMeta{UID: 1}, sealRewrite(l)); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(head)+string(rest) != body {
		t.Fatalf("the open reader read %d bytes, err %v", len(head)+len(rest), err)
	}
}

// The rename happens while this process holds the folder's lock in the lock
// service: another process cannot take it at that moment.
func TestRewriteRenamesUnderTheFolderLock(t *testing.T) {
	dial := lockstest.Server(t)
	home := t.TempDir()
	mb := lockedUser(t, home, dial())
	other := dial()
	u := mb.(*userMailbox)
	saveNamed(t, mb, "INBOX", "Subject: s\r\n\r\nbody\r\n", 1, [16]byte{})
	held := false
	testBeforeRewriteRename = func() {
		ctx, cancel := context.WithTimeout(locks.WithSite(context.Background(), "test-other-pod"), time.Second)
		defer cancel()
		_, err := other.Lock(ctx, locks.MailboxKey("alice@example.com", "INBOX"), locks.Owner("alice@example.com", "another-pod"), 5*time.Second)
		held = errors.Is(err, locks.ErrBusy)
	}
	t.Cleanup(func() { testBeforeRewriteRename = nil })
	if err := u.RewriteRecord("INBOX", &mailbox.MessageMeta{UID: 1}, sealRewrite(rewriteLayer(t))); err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatal("another process could take the folder lock while the rename ran")
	}
}

func TestSweepRewriteTempsTakesOnlyOldOnes(t *testing.T) {
	_, u := rewriteUser(t)
	dir := u.folderPath("INBOX")
	old := filepath.Join(dir, rewritePrefix+"1.1")
	young := filepath.Join(dir, rewritePrefix+"1.2")
	save := filepath.Join(dir, temporaryPrefix+"x")
	for _, p := range []string{old, young, save} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Hour)
	for _, p := range []string{old, save} {
		_ = os.Chtimes(p, past, past)
	}
	n, err := u.SweepRewriteTemps("INBOX", time.Minute)
	if err != nil || n != 1 {
		t.Fatalf("removed %d err %v", n, err)
	}
	for p, want := range map[string]bool{old: false, young: true, save: true} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s: present %v, want %v", filepath.Base(p), err == nil, want)
		}
	}
}

// A body in the reference format becomes age under the current key, with the
// same plaintext by sha256 and the record's entries kept.
func TestRewriteFromTheReferenceFormat(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "mailcrypt", "refstream", "testdata", "reference")
	stream, err := os.ReadFile(filepath.Join(dir, "prime256v1-gcm-1000.bin"))
	if err != nil {
		t.Fatal(err)
	}
	pemKey, err := os.ReadFile(filepath.Join(dir, "prime256v1.pem"))
	if err != nil {
		t.Fatal(err)
	}
	refKey, err := refstream.ParsePrivateKeyPEM(pemKey)
	if err != nil {
		t.Fatal(err)
	}
	const plainSHA = "da8b3e54bc424913119984df6bc33e2e629e4a5bc695e375558ca1e3ec66fdb1"
	id, _ := agelib.GenerateX25519Identity()
	l := mailcrypt.New(mailcrypt.Options{
		Recipients:    []agelib.Recipient{id.Recipient()},
		Identities:    []agelib.Identity{id},
		ReferenceKeys: []*ecdh.PrivateKey{refKey},
	})

	_, u := rewriteUser(t)
	path := filepath.Join(u.folderPath("INBOX"), "u.1")
	var file bytes.Buffer
	file.Write(encodeFileHeaderLine(0x5f000000))
	file.Write(encodeMessageHeader(messageHeader{Size: uint64(len(stream))}))
	file.Write(stream)
	file.Write(encodeMetadataBlock([]metadataEntry{
		{Key: metaKeyReceived, Value: "5f000000"},
		{Key: metaKeyVirtualSize, Value: "3e8"},
		{Key: metaKeyGUID, Value: strings.Repeat("ab", 16)},
	}))
	if err := os.WriteFile(path, file.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{UID: 1}
	if err := u.RewriteRecord("INBOX", m, sealRewrite(l)); err != nil {
		t.Fatal(err)
	}
	raw, err := u.RawRecord("INBOX", m)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := l.Probe(raw); err != nil || st != mailcrypt.StateCurrent {
		t.Fatalf("state %v err %v", st, err)
	}
	o, err := l.Open(raw, raw)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(o)
	_ = o.Close()
	if sum := sha256.Sum256(plain); hex.EncodeToString(sum[:]) != plainSHA {
		t.Fatal("the plaintext changed")
	}
	meta := metaOf(t, path)
	if meta[metaKeyGUID] != strings.Repeat("ab", 16) || meta[metaKeyReceived] != "5f000000" || meta[metaKeyVirtualSize] != "3e8" {
		t.Fatalf("entries %v", meta)
	}
	if meta[metaKeyPhysicalSize] != "3e8" {
		t.Fatalf("Z %q, want the plaintext's 1000", meta[metaKeyPhysicalSize])
	}
}

func TestRewriteRefusesWithoutTheLockService(t *testing.T) {
	_, mb, _ := newTestUser(t)
	saveNamed(t, mb, "INBOX", "Subject: s\r\n\r\nbody\r\n", 1, [16]byte{})
	u := mb.(*userMailbox)
	path := filepath.Join(u.folderPath("INBOX"), "u.1")
	before, _ := os.ReadFile(path)
	if err := u.RewriteRecord("INBOX", &mailbox.MessageMeta{UID: 1}, sealRewrite(rewriteLayer(t))); err == nil {
		t.Fatal("a rewrite ran with no lock service")
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("the file changed")
	}
	noTemps(t, u.folderPath("INBOX"))
}

// Until the rename the temp wears its own write time, so the sweep's age
// check sees a live rewrite's file as young, whatever the message's date.
func TestRewriteTempIsYoungUntilTheRename(t *testing.T) {
	mb, u := rewriteUser(t)
	saveNamed(t, mb, "INBOX", "Subject: s\r\n\r\nbody\r\n", 1, [16]byte{})
	path := filepath.Join(u.folderPath("INBOX"), "u.1")
	lastYear := time.Now().AddDate(-1, 0, 0)
	if err := os.Chtimes(path, lastYear, lastYear); err != nil {
		t.Fatal(err)
	}
	swept := -1
	testAfterRewriteVerified = func() {
		swept, _ = u.SweepRewriteTemps("INBOX", time.Minute)
	}
	t.Cleanup(func() { testAfterRewriteVerified = nil })
	if err := u.RewriteRecord("INBOX", &mailbox.MessageMeta{UID: 1}, sealRewrite(rewriteLayer(t))); err != nil {
		t.Fatal(err)
	}
	if swept != 0 {
		t.Fatalf("the sweep took %d live temps", swept)
	}
	if st, _ := os.Stat(path); !st.ModTime().Equal(lastYear) {
		t.Fatalf("the message is dated %v, want %v", st.ModTime(), lastYear)
	}
}
