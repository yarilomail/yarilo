package loginproto

import (
	"bufio"
	"errors"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/lineio"
)

func TestAPreambleWithoutAnEndIsRefused(t *testing.T) {
	_, err := Parse(bufio.NewReader(strings.NewReader(strings.Repeat("A", 8<<20))))
	if !errors.Is(err, lineio.ErrTooLong) {
		t.Fatalf("Parse = %v, want ErrTooLong", err)
	}
}
