package protocol

import (
	"context"
	"net"
	"testing"

	"github.com/yarilomail/yarilo/pkg/lineio/linetest"
)

func TestTheAuthServicesCutAnEndlessLine(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go NewServer(nil).Serve(ctx, ln) //nolint:errcheck
	t.Run("client protocol", func(t *testing.T) { linetest.ExpectCutOff(t, ln.Addr().String(), nil, "") })
	t.Run("master protocol", func(t *testing.T) { linetest.ExpectCutOff(t, serveMaster(t, nil), nil, "") })
}
