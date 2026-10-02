package dictsrv

import (
	"context"
	"net"
	"testing"

	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/lineio/linetest"
)

func TestTheDictServerCutsAnEndlessLine(t *testing.T) {
	real, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go New(map[string]dict.Dict{"d": real}, nil).Serve(ctx, ln) //nolint:errcheck
	linetest.ExpectCutOff(t, ln.Addr().String(), nil, "")
}
