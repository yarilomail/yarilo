package warden_test

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/lineio/linetest"
)

func TestTheWardenCutsAnEndlessLine(t *testing.T) {
	addr, cancel := startServer(t, 10)
	t.Cleanup(cancel)
	linetest.ExpectCutOff(t, addr, nil, "")
}
