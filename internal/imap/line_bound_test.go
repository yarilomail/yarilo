package imap

import (
	"io"
	"net"
	"testing"

	"github.com/yarilomail/yarilo/pkg/lineio/linetest"
)

// The line limit is held while reading, not checked once the line is in.
func TestTheIMAPLineLimitCutsAnEndlessLine(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ml := &maxLineLenListener{Listener: ln, limit: 8192}
	go func() {
		c, err := ml.Accept()
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, c)
		c.Close() //nolint:errcheck
	}()
	linetest.ExpectCutOff(t, ln.Addr().String(), nil, "")
}
