package locks

import (
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/lineio/linetest"
)

func TestTheLocksServerCutsAnEndlessLine(t *testing.T) {
	addr, _ := standAddr(t, NewMemoryBackend())
	linetest.ExpectCutOff(t, addr, nil, "")
}
