package protocol

import (
	"context"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/lineio/linetest"
)

func TestTheAuthServicesCutAnEndlessLine(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go NewServer(nil).ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)
	t.Run("client protocol", func(t *testing.T) { linetest.ExpectCutOff(t, addr, nil, "") })
	t.Run("master protocol", func(t *testing.T) { linetest.ExpectCutOff(t, serveMaster(t, nil), nil, "") })
}
