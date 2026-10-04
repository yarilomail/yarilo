package ftsproto_test

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/lineio/linetest"
)

func TestTheFTSServerCutsAnEndlessLine(t *testing.T) {
	addr, _ := serveSlow(t)
	linetest.ExpectCutOff(t, addr, nil, "")
}
