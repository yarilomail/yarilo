package mailboxbuild

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A namespace whose location names the virtual driver gets the virtual
// backend: without this the namespace would be served by the global driver
// and every configuration directory would read as an empty mailbox.
func TestVirtualDriverIsBuilt(t *testing.T) {
	b := ByDriver("virtual", config.StorageConfig{}, nil)
	if b == nil {
		t.Fatal("no backend for the virtual driver")
	}
	u := b.OpenUser(&mailbox.UserInfo{Username: "u@test", Home: t.TempDir(), MailPath: t.TempDir(), Separator: "/"})
	if err := u.Create("Whatever"); err == nil {
		t.Error("the backend built for virtual accepted CREATE, so it is not the virtual one")
	}
}
