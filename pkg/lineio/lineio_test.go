package lineio

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

// countingReader counts what the line reader pulled from below it.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestReadLine(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		max            int
		err            error
	}{
		{"short", "abc\nrest", "abc\n", 16, nil},
		{"at the bound", strings.Repeat("a", 15) + "\n", strings.Repeat("a", 15) + "\n", 16, nil},
		{"one over", strings.Repeat("a", 16) + "\n", strings.Repeat("a", 16), 16, ErrTooLong},
		{"longer than the buffer", strings.Repeat("a", 9000) + "\n", strings.Repeat("a", 9000) + "\n", 10000, nil},
		{"eof", "abc", "abc", 16, io.EOF},
	} {
		got, err := ReadLine(bufio.NewReaderSize(strings.NewReader(tc.in), 4096), tc.max)
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("%s: got %d bytes, %v; want %d, %v", tc.name, len(got), err, len(tc.want), tc.err)
		}
	}
}

// 8 MiB without a newline is refused near the bound, not read whole.
func TestAnEndlessLineStopsNearTheBound(t *testing.T) {
	src := &countingReader{r: strings.NewReader(strings.Repeat("a", 8<<20))}
	if _, err := ReadLine(bufio.NewReaderSize(src, 4096), 65536); !errors.Is(err, ErrTooLong) {
		t.Fatalf("err = %v, want ErrTooLong", err)
	}
	if src.n > 65536+4096 {
		t.Fatalf("read %d bytes for a 65536 bound", src.n)
	}
}
