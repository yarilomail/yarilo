package sasllogin_test

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/sasllogin"
	"github.com/yarilomail/yarilo/pkg/lineio/linetest"
)

func TestTheSASLProxyCutsAnEndlessLine(t *testing.T) {
	fa := startFakeAuth(t)
	addr := startProxy(t, fa.ln.Addr().String(), sasllogin.Options{})
	linetest.ExpectCutOff(t, addr, nil, "")
}
