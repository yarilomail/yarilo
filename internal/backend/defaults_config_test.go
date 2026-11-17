package backend

import (
	"reflect"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// The layout config.Defaults writes is the one an empty value resolved to.
func TestDefaultsAreTheEmptyLayout(t *testing.T) {
	def := config.Defaults()
	empty := config.Defaults()
	empty.Storage.MaildirRoot, empty.Storage.MailHome = "", ""
	if a, b := BuildResolver(empty), BuildResolver(def); !reflect.DeepEqual(a, b) {
		t.Fatalf("defaults resolve to %+v, empty to %+v", b, a)
	}
}
