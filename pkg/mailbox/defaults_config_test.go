package mailbox_test

import (
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

func TestDefaultsAreTheEmptyFsync(t *testing.T) {
	empty, _ := mailbox.ParseFsyncMode("")
	if got := config.Defaults().Storage.MailFsync; got != string(empty) {
		t.Fatalf("defaults %q, empty gave %q", got, empty)
	}
}
