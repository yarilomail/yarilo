package locks

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/lineio/linetest"
)

func TestTheLocksServerCutsAnEndlessLine(t *testing.T) {
	addr, _ := standAddr(t, NewMemoryBackend())
	linetest.ExpectCutOff(t, addr, nil, "")
}
