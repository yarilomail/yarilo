package sieve

import (
	"fmt"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

func TestDefaultsAreTheEmptyScriptsDriver(t *testing.T) {
	empty := NewScriptStore("", "", nil, nil)
	def := NewScriptStore(config.Defaults().Sieve.ScriptsDriver, "", nil, nil)
	if a, b := fmt.Sprintf("%T", empty.ScriptStore), fmt.Sprintf("%T", def.ScriptStore); a != b {
		t.Fatalf("defaults build %s, empty builds %s", b, a)
	}
}
