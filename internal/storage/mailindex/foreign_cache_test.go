package mailindex

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// foreignCachePath is a cache file a reference install wrote, copied off the
// sandbox (the recipe is in internal/imaptext/testdata/README.md).
const foreignCachePath = "../index/file/testdata/foreign-cache/dovecot.index.cache"

func foreignPair(t *testing.T) (indexID, fileSeq uint32) {
	t.Helper()
	raw, err := os.ReadFile(foreignCachePath)
	if err != nil {
		t.Fatal(err)
	}
	le := binary.LittleEndian
	return le.Uint32(raw[4:]), le.Uint32(raw[8:])
}

// A cache another implementation wrote opens and its field table reads. Two
// things had to be true for that: byte 0 in the producer slot is accepted, and
// the header's field_header_offset is packed the way the reference packs it
// (mail-index-util.c:21-31) rather than written plain.
func TestAForeignCacheFileReads(t *testing.T) {
	indexID, fileSeq := foreignPair(t)
	cf, err := OpenCache(foreignCachePath, indexID, fileSeq)
	if err != nil {
		t.Fatalf("open a reference-written cache: %v", err)
	}
	defer cf.Close() //nolint:errcheck

	names := make([]string, 0, len(cf.Fields()))
	for _, f := range cf.Fields() {
		names = append(names, f.Name)
	}
	if len(names) == 0 {
		t.Fatal("the field table read empty")
	}
	for _, want := range []string{"flags", "date.sent", "size.physical", "imap.bodystructure", "guid"} {
		if !namesHave(names, want) {
			t.Errorf("the table does not name %q: %v", want, names)
		}
	}
	// It caches headers and builds the envelope from them, so a reader of
	// theirs must not assume imap.envelope is present.
	if namesHave(names, "imap.envelope") {
		t.Log("this fixture happens to carry imap.envelope too")
	}
	var headers int
	for _, n := range names {
		if strings.HasPrefix(strings.ToLower(n), "hdr.") {
			headers++
		}
	}
	if headers == 0 {
		t.Error("no cached header fields, so the fixture is not the shape a reference cache has")
	}
}

// The header offset is the one field we wrote plain while the reference packs
// it. A file of ours must survive the round trip, or our own caches stop
// reading the moment the encoding changed.
func TestOurOwnCacheStillReadsAfterThePacking(t *testing.T) {
	path := filepath.Join(t.TempDir(), CacheFileName)
	cf, err := CreateCache(path, 7, 9)
	if err != nil {
		t.Fatal(err)
	}
	first, err := cf.AddFields([]CacheField{{Name: "imap.envelope", Type: CacheFieldString, Decision: CacheDecisionYes}})
	if err != nil {
		t.Fatal(err)
	}
	off, err := cf.AppendRecord(0, []CacheFieldValue{{FieldID: first, Data: []byte("NIL")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := cf.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenCache(path, 7, 9)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close() //nolint:errcheck
	id, ok := reopened.FieldID("imap.envelope")
	if !ok {
		t.Fatal("our own field table did not read back")
	}
	vals, err := reopened.ReadRecord(off)
	if err != nil {
		t.Fatalf("read back our own record: %v", err)
	}
	if string(vals[id]) != "NIL" {
		t.Errorf("record value = %q", vals[id])
	}
}

func namesHave(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
