package pop3

import (
	"io"
	"net"
	"strings"
	"testing"
)

func TestAPOP3SessionCutsAnEndlessLine(t *testing.T) {
	c, br := newPOP3Session(t, newTestOpts(t, &mockAuth{}, nil, nil))
	go c.Write([]byte(strings.Repeat("A", 8<<20))) //nolint:errcheck
	if _, err := io.Copy(io.Discard, br); err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatal("the session still holds the connection after 8 MiB with no line end")
		}
	}
}
