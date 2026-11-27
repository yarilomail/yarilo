package integration_test

import (
	"bytes"
	"crypto/ecdh"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/yarilomail/yarilo/v2/internal/mailcrypt"
	"github.com/yarilomail/yarilo/v2/internal/mailcrypt/refstream"
	indexfile "github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

type cryptDriver struct {
	name string
	open func(*mailcrypt.Layer, *mailbox.UserInfo) mailbox.UserMailbox
}

var cryptDrivers = []cryptDriver{
	{"maildir", func(l *mailcrypt.Layer, i *mailbox.UserInfo) mailbox.UserMailbox {
		return maildir.New(maildir.WithCrypt(l)).OpenUser(i)
	}},
	{"sdbox", func(l *mailcrypt.Layer, i *mailbox.UserInfo) mailbox.UserMailbox {
		return dboxv2.New(dboxv2.WithCrypt(l)).OpenUser(i)
	}},
	{"mdbox", func(l *mailcrypt.Layer, i *mailbox.UserInfo) mailbox.UserMailbox {
		return mdbox.New(mdbox.WithCrypt(l)).OpenUser(i)
	}},
}

const cryptMarker = "a sentence only the body carries"

// TestBodiesAtRest: with the layer writing age, every driver stores
// ciphertext, gives the plaintext back, and reports the plaintext's sizes;
// without it the same store still reads, and planted ciphertext is refused
// either way.
func TestBodiesAtRest(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	enc := mailcrypt.New(mailcrypt.Options{Recipients: []age.Recipient{id.Recipient()}, Identities: []age.Identity{id}})
	readOnly := mailcrypt.New(mailcrypt.Options{Identities: []age.Identity{id}})
	body := "From: a@b\r\nSubject: s\r\n\r\n" + strings.Repeat(cryptMarker+"\r\n", 3000)

	for _, d := range cryptDrivers {
		t.Run(d.name, func(t *testing.T) {
			home := t.TempDir()
			info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: d.name}
			box := d.open(enc, info)
			if err := box.Init(); err != nil {
				t.Fatal(err)
			}
			idx := indexfile.New().OpenUser(info)
			defer idx.Close() //nolint:errcheck
			f, err := idx.OpenFolder("INBOX", 1)
			if err != nil {
				t.Fatal(err)
			}
			saved, vsize, guid, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
			if err != nil {
				t.Fatal(err)
			}
			if vsize != uint32(len(body)) {
				t.Fatalf("vsize %d, plaintext %d", vsize, len(body))
			}
			m := &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid}
			if err := mailboxbase.RecordSaved(idx, box, f.ID, "INBOX", saved, m); err != nil {
				t.Fatal(err)
			}

			if leaked := filesHolding(t, home, cryptMarker); len(leaked) > 0 {
				t.Fatalf("plaintext body on disk: %v", leaked)
			}
			if sealed := filesHolding(t, home, "age-encryption.org/v1\n"); len(sealed) == 0 {
				t.Fatal("no age file on disk")
			}

			for _, l := range []struct {
				name  string
				layer *mailcrypt.Layer
			}{{"writing", enc}, {"reading only", readOnly}} {
				b := d.open(l.layer, info)
				got := readRecord(t, b, m)
				if got != body {
					t.Fatalf("%s: plaintext differs (%d bytes, want %d)", l.name, len(got), len(body))
				}
				if rs, ok := mailbox.Driver(b).(mailbox.RecordSizer); ok {
					probe := *m
					probe.Size, probe.VSize = 0, 0
					size, vs, err := rs.RecordSize("INBOX", &probe)
					if err != nil || size != uint32(len(body)) || vs != uint32(len(body)) {
						t.Fatalf("%s: RecordSize %d/%d %v, want %d", l.name, size, vs, err, len(body))
					}
				}
				b.Close() //nolint:errcheck
			}

			for _, planted := range []string{"age-encryption.org/v1\n-> X25519 x\n", "CRYPTED\x03\x07\x02rest"} {
				_, _, _, err := box.Save("INBOX", strings.NewReader(planted), 0, int64(len(planted)), nil, nil, [16]byte{})
				if !errors.Is(err, mailcrypt.ErrPlanted) {
					t.Fatalf("planted %q: %v", planted[:8], err)
				}
			}
			box.Close() //nolint:errcheck
		})
	}
}

func readRecord(t *testing.T, box mailbox.UserMailbox, m *mailbox.MessageMeta) string {
	t.Helper()
	ua, ok := mailbox.Driver(box).(mailbox.UIDAddressable)
	if !ok {
		t.Fatal("driver cannot open by uid")
	}
	rc, err := ua.OpenRecord("INBOX", m)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close() //nolint:errcheck
	if _, ok := rc.(mailbox.RangeReader); !ok {
		t.Fatalf("%T: a sealed body is not readable at an offset", rc)
	}
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func filesHolding(t *testing.T, root, needle string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr == nil && bytes.Contains(b, []byte(needle)) {
			out = append(out, strings.TrimPrefix(p, root))
		}
		return nil
	})
	return out
}

// TestMoveKeepsASealedBodyReadable: a move inside the store (rename, or the
// mdbox re-save) leaves the body readable as its plaintext.
func TestMoveKeepsASealedBodyReadable(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	enc := mailcrypt.New(mailcrypt.Options{Recipients: []age.Recipient{id.Recipient()}, Identities: []age.Identity{id}})
	body := "From: a@b\r\n\r\n" + cryptMarker + "\r\n"
	for _, d := range cryptDrivers {
		t.Run(d.name, func(t *testing.T) {
			info := &mailbox.UserInfo{Username: "u1@example.com", Home: t.TempDir(), Driver: d.name}
			box := d.open(enc, info)
			defer box.Close() //nolint:errcheck
			if err := box.Init(); err != nil {
				t.Fatal(err)
			}
			if err := box.Create("Archive"); err != nil {
				t.Fatal(err)
			}
			idx := indexfile.New().OpenUser(info)
			defer idx.Close() //nolint:errcheck
			f, err := idx.OpenFolder("INBOX", 1)
			if err != nil {
				t.Fatal(err)
			}
			saved, vsize, guid, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
			if err != nil {
				t.Fatal(err)
			}
			m := &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid}
			if err := mailboxbase.RecordSaved(idx, box, f.ID, "INBOX", saved, m); err != nil {
				t.Fatal(err)
			}
			name, err := mailboxbase.MessagePath(box, "INBOX", m)
			if err != nil {
				t.Fatal(err)
			}
			moved, _, err := box.Move("INBOX", "Archive", name, guid)
			if err != nil {
				t.Fatal(err)
			}
			var rc io.ReadCloser
			if d.name == "maildir" {
				// Into tmp/ until the naming step publishes it (#1736).
				rc, err = enc.OpenFile(filepath.Join(info.Home, "Maildir", ".Archive", "tmp", moved))
			} else {
				rc, err = box.Fetch("Archive", moved, false)
			}
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(rc)
			rc.Close() //nolint:errcheck
			if string(got) != body {
				t.Fatalf("moved body differs: %q", got)
			}
		})
	}
}

// TestMixedMaildirFolder: plaintext, age and a reference-format file taken
// from another server's disk, the last two with no S= in their names, sit in
// one folder: each reads as its plaintext and lists at the plaintext's size.
func TestMixedMaildirFolder(t *testing.T) {
	vecDir := filepath.Join("..", "..", "mailcrypt", "refstream", "testdata", "reference")
	refBody, err := os.ReadFile(filepath.Join(vecDir, "prime256v1-gcm-1000.bin"))
	if err != nil {
		t.Fatal(err)
	}
	pemKey, err := os.ReadFile(filepath.Join(vecDir, "prime256v1.pem"))
	if err != nil {
		t.Fatal(err)
	}
	refKey, err := refstream.ParsePrivateKeyPEM(pemKey)
	if err != nil {
		t.Fatal(err)
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	layer := mailcrypt.New(mailcrypt.Options{
		Recipients: []age.Recipient{id.Recipient()}, Identities: []age.Identity{id},
		ReferenceKeys: []*ecdh.PrivateKey{refKey},
	})
	plain := []byte("From: a@b\r\n\r\nplain body\r\n")
	var sealedBuf bytes.Buffer
	if err := layer.Seal(&sealedBuf, strings.NewReader("From: a@b\r\n\r\nage body\r\n")); err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	box := maildir.New(maildir.WithCrypt(layer)).OpenUser(info)
	defer box.Close() //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	cur := filepath.Join(home, "Maildir", "cur")
	files := map[string][]byte{
		"1000.M1P1.host:2,S": plain,
		"1001.M1P1.host:2,S": sealedBuf.Bytes(),
		"1002.M1P1.host:2,S": refBody,
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(cur, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	wantLen := map[string]int{"1000.M1P1.host:2,S": len(plain), "1001.M1P1.host:2,S": len("From: a@b\r\n\r\nage body\r\n"), "1002.M1P1.host:2,S": 1000}
	// What a read serves: the reference vector's four bare LF go out as CRLF,
	// the length RFC822.SIZE gives (#2581); the listed size stays physical.
	wantRead := map[string]int{"1000.M1P1.host:2,S": len(plain), "1001.M1P1.host:2,S": len("From: a@b\r\n\r\nage body\r\n"), "1002.M1P1.host:2,S": 1004}

	recs, err := box.Scan("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("%d records", len(recs))
	}
	for _, r := range recs {
		if int(r.Size) != wantLen[r.Filename] {
			t.Errorf("%s: listed at %d, plaintext %d", r.Filename, r.Size, wantLen[r.Filename])
		}
		rc, err := box.Fetch("INBOX", r.Filename, false)
		if err != nil {
			t.Fatalf("%s: %v", r.Filename, err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close() //nolint:errcheck
		if len(got) != wantRead[r.Filename] || mailcrypt.Classify(got) != mailcrypt.Plain {
			t.Errorf("%s: read %d bytes, kind %v", r.Filename, len(got), mailcrypt.Classify(got))
		}
	}
}

// A sealed body whose size cannot be told lists at 0, unknown, never at the
// ciphertext's length, which would reach the index as the message's size.
func TestUnknownPlainSizeIsNotTheCiphertexts(t *testing.T) {
	vecDir := filepath.Join("..", "..", "mailcrypt", "refstream", "testdata", "reference")
	refBody, err := os.ReadFile(filepath.Join(vecDir, "prime256v1-gcm-1000.bin"))
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	box := maildir.New(maildir.WithCrypt(mailcrypt.New(mailcrypt.Options{}))).OpenUser(info)
	defer box.Close() //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "Maildir", "cur", "1002.M1P1.host:2,S"), refBody, 0o600); err != nil {
		t.Fatal(err)
	}
	recs, err := box.Scan("INBOX")
	if err != nil || len(recs) != 1 {
		t.Fatalf("%d records, %v", len(recs), err)
	}
	if recs[0].Size == uint32(len(refBody)) || recs[0].Size != 0 {
		t.Fatalf("listed at %d, the file is %d bytes: want 0, unknown", recs[0].Size, len(refBody))
	}
}
