package mailindex

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writtenCache holds one field and two records, the second continued behind
// the first, and reports where the field table and the second record sit.
func writtenCache(t *testing.T) (path string, table, second uint32) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "yarilo.index.cache")
	c, err := CreateCache(path, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck
	fid, err := c.AddFields([]CacheField{{Name: "probe", Type: CacheFieldVariableSize, Decision: CacheDecisionYes}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := c.AppendRecord(0, []CacheFieldValue{{FieldID: fid, Data: []byte("one")}})
	if err != nil {
		t.Fatal(err)
	}
	if second, err = c.AppendRecord(first, []CacheFieldValue{{FieldID: fid, Data: []byte("two")}}); err != nil {
		t.Fatal(err)
	}
	return path, c.Header().FieldHeaderOffset, second
}

func patch(t *testing.T, path string, at uint32, v uint32) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	if _, err := f.WriteAt(b[:], int64(at)); err != nil {
		t.Fatal(err)
	}
}

// A cache whose field table is damaged does not open: every record would
// decode against fields it does not have.
func TestACacheWithADamagedFieldTableDoesNotOpen(t *testing.T) {
	for name, damage := range map[string]func(path string, table uint32){
		"table smaller than its fixed part": func(p string, at uint32) { patch(t, p, at+4, 5) },
		"more fields than the table holds":  func(p string, at uint32) { patch(t, p, at+8, 1000) },
		"table past the end of the file": func(p string, _ uint32) {
			h := CacheHeader{MajorVersion: 1, CompatSizeofUoffT: 4, IndexID: 1, FileSeq: 2, FieldHeaderOffset: 1 << 20}
			raw, _ := os.ReadFile(p)
			copy(raw, h.encode())
			_ = os.WriteFile(p, raw, 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			path, table, _ := writtenCache(t)
			damage(path, table)
			if c, err := OpenCache(path, 1, 2); !errors.Is(err, ErrCacheInvalid) {
				if c != nil {
					_ = c.Close()
				}
				t.Errorf("open: %v, want an invalid cache", err)
			}
		})
	}
}

// A damaged record reads as an invalid cache, never as a shorter record: the
// caller then rebuilds it from the message.
func TestADamagedCacheRecordIsInvalid(t *testing.T) {
	for name, damage := range map[string]func(path string, at uint32){
		"size below its header": func(p string, at uint32) { patch(t, p, at+4, 4) },
		"size past the file":    func(p string, at uint32) { patch(t, p, at+4, 1<<20) },
		"chain onto itself":     func(p string, at uint32) { patch(t, p, at, at) },
		"unknown field":         func(p string, at uint32) { patch(t, p, at+8, 9) },
	} {
		t.Run(name, func(t *testing.T) {
			path, _, second := writtenCache(t)
			damage(path, second)
			c, err := OpenCache(path, 1, 2)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close() //nolint:errcheck
			if _, err := c.ReadRecord(second); !errors.Is(err, ErrCacheInvalid) {
				t.Errorf("read: %v, want an invalid cache", err)
			}
		})
	}
}

// Fields already in the table are not added again, and a record naming a field
// the table lacks is refused before anything is written.
func TestACacheAddsOnlyNewFieldsAndRefusesUnknownOnes(t *testing.T) {
	path, _, _ := writtenCache(t)
	c, err := OpenCache(path, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck
	before, _ := os.Stat(path)
	if _, err := c.AddFields([]CacheField{{Name: "PROBE", Type: CacheFieldVariableSize, Decision: CacheDecisionYes}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AppendRecord(0, []CacheFieldValue{{FieldID: 7, Data: []byte("x")}}); err == nil {
		t.Error("a record naming field 7 of 1 was written")
	}
	if after, _ := os.Stat(path); after.Size() != before.Size() {
		t.Errorf("the cache grew %d -> %d", before.Size(), after.Size())
	}
	c.Preload()
	c.Preload()
	if len(c.Fields()) != 1 {
		t.Errorf("%d fields, want 1", len(c.Fields()))
	}
}
