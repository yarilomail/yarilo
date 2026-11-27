package mdbox

import (
	"os"
	"path/filepath"
	"testing"
)

// A rewrite (purge, alt move) keeps V and Z from the old trailer: a reference
// format body, or a bare-LF one, cannot tell its plaintext size by its length.
func TestCompactionKeepsTheTrailerSizes(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		v, z uint32
	}{
		{"reference format", append([]byte("CRYPTED\x03\x07\x02"), make([]byte, 1200)...), 1000, 1000},
		{"bare LF", []byte("From: a@b\n\nline\nline\n"), 25, 0},
	}
	for _, c := range cases {
		dir := t.TempDir()
		src, err := os.Create(filepath.Join(dir, "m.1"))
		if err != nil {
			t.Fatal(err)
		}
		off, err := appendRecordToFile(src, c.body, parsedTrailer{guid: randomGUID(), origMailbox: "INBOX", vsize: c.v, physSize: c.z})
		if err != nil {
			t.Fatal(err)
		}
		body, tr, err := readRecordBodyAndTrailer(src, off)
		if err != nil {
			t.Fatal(err)
		}
		dst, err := os.Create(filepath.Join(dir, "m.2"))
		if err != nil {
			t.Fatal(err)
		}
		off2, err := appendRecordToFile(dst, body, tr)
		if err != nil {
			t.Fatal(err)
		}
		_, moved, err := readRecordBodyAndTrailer(dst, off2)
		if err != nil {
			t.Fatal(err)
		}
		if moved.vsize != c.v || moved.physSize != c.z {
			t.Errorf("%s: V=%d Z=%d after the rewrite, want V=%d Z=%d", c.name, moved.vsize, moved.physSize, c.v, c.z)
		}
		_ = src.Close()
		_ = dst.Close()
	}
}
