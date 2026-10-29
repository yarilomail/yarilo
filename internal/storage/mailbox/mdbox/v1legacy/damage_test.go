package v1legacy

import (
	"os"
	"path/filepath"
	"testing"
)

// A legacy map line that does not parse is skipped, a blank one too, and the
// entries around them read.
func TestALegacyMapSkipsLinesThatAreNotEntries(t *testing.T) {
	home := t.TempDir()
	off := writeV1Message(t, home, 1, []byte("one"), [16]byte{1}, 1700000000)
	appendMapLine(t, home, "INBOX", MapEntry{FileID: 1, Offset: off, Size: 3})
	f, err := os.OpenFile(MapPath(home, "INBOX"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("\nnot an entry\n")
	_ = f.Close()
	off2 := writeV1Message(t, home, 1, []byte("two"), [16]byte{2}, 1700000000)
	appendMapLine(t, home, "INBOX", MapEntry{FileID: 1, Offset: off2, Size: 3})
	entries, err := ReadMap(home, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("read %d entries, want the two around the junk", len(entries))
	}
}

// A legacy record that is not one is refused: bad magic, a size not a number,
// a body cut short, a missing file. One with no trailer reads whole.
func TestALegacyRecordIsReadOrRefused(t *testing.T) {
	home := t.TempDir()
	off := writeV1Message(t, home, 1, []byte("whole body"), [16]byte{1}, 1700000000)
	path := filepath.Join(StorageDir(home), "m.1")
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	e := MapEntry{FileID: 1, Offset: off, Size: 10}
	for name, damage := range map[string]func([]byte) []byte{
		"magic":        func(b []byte) []byte { b[0] = 'X'; return b },
		"size not hex": func(b []byte) []byte { copy(b[13:29], "zzzzzzzzzzzzzzzz"); return b },
		"body cut":     func(b []byte) []byte { return b[:headerLen+4] },
	} {
		if err := os.WriteFile(path, damage(append([]byte(nil), good...)), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadMessage(home, e); err == nil {
			t.Errorf("%s: read a damaged record", name)
		}
	}
	if _, err := ReadMessage(home, MapEntry{FileID: 9}); err == nil {
		t.Error("read from a storage file that is not there")
	}
	noTrailer := good[:headerLen+10]
	if err := os.WriteFile(path, noTrailer, 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := ReadMessage(home, e)
	if err != nil {
		t.Fatalf("record without a trailer: %v", err)
	}
	if string(m.Body) != "whole body" {
		t.Errorf("record without a trailer reads %q", m.Body)
	}
}
