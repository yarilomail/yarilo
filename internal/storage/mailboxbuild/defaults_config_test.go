package mailboxbuild

import (
	"fmt"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

func TestDefaultsAreTheEmptyDriver(t *testing.T) {
	sc := config.Defaults().Storage
	if a, b := fmt.Sprintf("%T", byDriver("", sc, nil)), fmt.Sprintf("%T", byDriver(sc.MailDriver, sc, nil)); a != b {
		t.Fatalf("defaults build %s, empty builds %s", b, a)
	}
}
