package sasllogin_test

import (
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/sasllogin"
	"github.com/yarilomail/yarilo/v2/pkg/lineio/linetest"
)

func TestTheSASLProxyCutsAnEndlessLine(t *testing.T) {
	fa := startFakeAuth(t)
	addr := startProxy(t, fa.ln.Addr().String(), sasllogin.Options{})
	linetest.ExpectCutOff(t, addr, nil, "")
}
