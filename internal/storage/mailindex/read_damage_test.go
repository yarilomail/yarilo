package mailindex

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writtenIndex is a base as Recreate writes it: one record extension and two
// records, so every region of the file has bytes to damage.
func writtenIndex(t *testing.T) []byte {
	t.Helper()
	f, err := NewFile(1717185600, []Extension{
		{Name: "modseq", HdrSize: 16, HdrData: make([]byte, 16), RecordSize: 8, RecordAlign: 8, ResetID: 7},
	})
	if err != nil {
		t.Fatal(err)
	}
	for uid := uint32(1); uid <= 2; uid++ {
		f.Records = append(f.Records, &Record{UID: uid, Ext: map[string][]byte{"modseq": make([]byte, 8)}})
	}
	f.Header.MessagesCount = 2
	f.Header.NextUID = 3
	path := filepath.Join(t.TempDir(), "yarilo.index")
	if _, err := Recreate(RecreateInput{Path: path, Header: f.Header, Extensions: f.Extensions, Records: f.Records}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Read(bytes.NewReader(raw)); err != nil {
		t.Fatalf("the undamaged base does not read: %v", err)
	}
	return raw
}

// A damaged base is refused with the error that names the damage, never read
// as a shorter or different index.
func TestADamagedBaseIsRefused(t *testing.T) {
	le := binary.LittleEndian
	for name, c := range map[string]struct {
		damage func([]byte) []byte
		want   error
	}{
		"header cut short":       {func(b []byte) []byte { return b[:50] }, ErrShortRead},
		"header size below base": {func(b []byte) []byte { le.PutUint32(b[4:], 10); return b }, ErrCorrupted},
		"extension headers cut":  {func(b []byte) []byte { return b[:HeaderMinSize+5] }, nil},
		"extension headers garbage": {func(b []byte) []byte {
			for i := HeaderMinSize; i < int(le.Uint32(b[4:])); i++ {
				b[i] = 0xff
			}
			return b
		}, nil},
		"zero record size":           {func(b []byte) []byte { le.PutUint32(b[8:], 0); return b }, ErrCorrupted},
		"record size off the layout": {func(b []byte) []byte { le.PutUint32(b[8:], le.Uint32(b[8:])+8); return b }, ErrCorrupted},
		"trailing partial record":    {func(b []byte) []byte { return append(b, 1, 2, 3) }, ErrShortRead},
	} {
		t.Run(name, func(t *testing.T) {
			f, err := Read(bytes.NewReader(c.damage(writtenIndex(t))))
			if err == nil {
				t.Fatalf("read %d records from a damaged base", len(f.Records))
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Errorf("err %v, want %v", err, c.want)
			}
		})
	}
}

// Recreate refuses an input that would write a base its own reader refuses,
// and leaves the old base where it was.
func TestRecreateRefusesABaseItCouldNotRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yarilo.index")
	if err := os.WriteFile(path, writtenIndex(t), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	good, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]RecreateInput{
		"no path": {Header: good.Header, Extensions: good.Extensions},
		"header size off the extensions": func() RecreateInput {
			h := good.Header
			h.HeaderSize += 8
			return RecreateInput{Path: path, Header: h, Extensions: good.Extensions, Records: good.Records}
		}(),
		"alignment no layout allows": {Path: path, Header: good.Header, Records: good.Records,
			Extensions: []Extension{{Name: "odd", RecordSize: 3, RecordAlign: 3}}},
	} {
		if _, err := Recreate(in); err == nil {
			t.Errorf("%s: Recreate accepted it", name)
		}
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Error("a refused Recreate changed the base")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("a refused Recreate left %d files, want the base alone", len(entries))
	}
}
