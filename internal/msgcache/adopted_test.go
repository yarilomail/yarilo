package msgcache

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// foreignFixture is a store a reference install wrote, copied off the sandbox
// (internal/storage/index/file/testdata/foreign-cache).
const foreignFixture = "../storage/index/file/testdata/foreign-cache"

func adoptedFolder(t *testing.T) (*mailbox.UserInfo, mailbox.UserIndex, *mailbox.Folder, *mailbox.MessageMeta) {
	t.Helper()
	home := t.TempDir()
	for src, rel := range map[string]string{
		"dovecot.index.log":     filepath.Join("index", "mailboxes", "Fixture", "dovecot.index.log"),
		"dovecot.index.cache":   filepath.Join("index", "mailboxes", "Fixture", "dovecot.index.cache"),
		"dovecot.map.index":     filepath.Join("index", "storage", "dovecot.map.index"),
		"dovecot.map.index.log": filepath.Join("index", "storage", "dovecot.map.index.log"),
		"m.1":                   filepath.Join("mdbox", "storage", "m.1"),
	} {
		b, err := os.ReadFile(filepath.Join(foreignFixture, src))
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	info := &mailbox.UserInfo{
		Username: "u1@d00001.test", Home: home, Driver: "mdbox",
		IndexDir: filepath.Join(home, "index"),
	}
	idx := file.New().OpenUser(info)
	f, err := idx.OpenFolder("Fixture", 1)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	msgs, err := idx.GetMessages(f.ID, nil)
	if err != nil || len(msgs) == 0 {
		t.Fatalf("messages: %v", err)
	}
	for _, m := range msgs {
		if m.CacheOffset != 0 {
			return info, idx, f, m
		}
	}
	t.Fatal("no record points into the adopted cache")
	return nil, nil, nil, nil
}

func adoptedCachePath(info *mailbox.UserInfo) string {
	return filepath.Join(info.Home, "index", "mailboxes", "Fixture", "dbox-Mails", mailindex.CacheFileName)
}

// The adopted cache answers: the envelope comes out of the headers their
// server cached, with no message opened and nothing of ours written first.
func TestAnAdoptedCacheAnswersFromTheirRecords(t *testing.T) {
	info, idx, f, m := adoptedFolder(t)
	defer idx.Close() //nolint:errcheck
	_ = info

	fc := Open(idx, f.ID, Options{User: "u1@d00001.test", Folder: f.Name})
	if fc == nil {
		t.Fatal("the adopted cache did not open")
	}
	defer fc.Close()
	text, ok := fc.EnvelopeText(m)
	if !ok {
		t.Fatal("their record answered no envelope")
	}
	if text == "" {
		t.Error("the envelope came back empty")
	}
	if bs := fc.BodyStructure(m); bs == nil {
		t.Error("their body structure did not read")
	}
}

// A record bent after adoption is a miss on the first read, because the
// checksum was computed when the folder was taken over rather than when we
// first wrote into it.
func TestABentAdoptedRecordIsAMiss(t *testing.T) {
	info, idx, _, m := adoptedFolder(t)

	// Nothing of ours is read or written first: the checksum under test is the
	// one adoption computed, and a Handle would replace it with our own.
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	path := adoptedCachePath(info)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Inside a value, not in the framing: a bent length would fail the read
	// on its own and say nothing about the checksum.
	size := int(binary.LittleEndian.Uint32(raw[int(m.CacheOffset)+4:]))
	at := int(m.CacheOffset) + size - 3
	if size < 16 || at >= len(raw) {
		t.Fatalf("record at %d is %d bytes, file is %d", m.CacheOffset, size, len(raw))
	}
	raw[at] ^= 0xff
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	again := file.New().OpenUser(info)
	defer again.Close() //nolint:errcheck
	f2, err := again.OpenFolder("Fixture", 1)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := again.GetMessages(f2.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	var bent *mailbox.MessageMeta
	for _, msg := range msgs {
		if msg.UID == m.UID {
			bent = msg
		}
	}
	if bent == nil {
		t.Fatal("the record vanished")
	}
	if bent.CacheCRC == 0 {
		t.Fatal("the record lost its checksum, so the miss would prove nothing")
	}

	reader := Open(again, f2.ID, Options{User: "u1@d00001.test", Folder: f2.Name})
	if reader == nil {
		t.Fatal("cache unavailable")
	}
	defer reader.Close()
	was := testutil.ToFloat64(metricCRCMismatch)
	if vals := reader.read(bent); len(vals) != 0 {
		t.Errorf("a bent record was served: %d fields", len(vals))
	}
	if now := testutil.ToFloat64(metricCRCMismatch); now != was+1 {
		t.Errorf("mismatch counter = %v, want %v", now, was+1)
	}
}
