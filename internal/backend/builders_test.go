package backend

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// The resolver every binary builds takes the storage keys as written, fills
// only the root and home the file leaves out, and the separator from the
// personal namespace.
func TestBuildResolverReadsTheStorageKeys(t *testing.T) {
	cfg := config.Defaults()
	cfg.Storage.MaildirRoot, cfg.Storage.MailHome = "", ""
	cfg.Namespaces = nil
	r := BuildResolver(cfg)
	if r.Root != "/var/mail/vhosts" || r.HomeTemplate != "%d/%u" || r.DefaultSeparator != "." {
		t.Errorf("unset keys: root %q, home %q, separator %q", r.Root, r.HomeTemplate, r.DefaultSeparator)
	}

	cfg.Storage.MaildirRoot, cfg.Storage.MailHome = "/srv/mail", "%d/%n"
	cfg.Storage.MailIndexPath, cfg.Storage.MailControlPath = "/idx/%u", "/ctl/%u"
	cfg.Storage.MailVolatilePath, cfg.Storage.MailAltPath, cfg.Storage.MailPath = "/vol/%u", "/alt/%u", "/mail/%u"
	cfg.Storage.MailboxListStorageEscapeChar = "%"
	cfg.Storage.MailboxListNormalizeNamesToNFC = false
	cfg.Namespaces = []config.NamespaceConfig{{Type: "shared", Separator: "."}, {Type: "Personal", Separator: "/"}}
	r = BuildResolver(cfg)
	want := mailbox.Resolver{
		Root: "/srv/mail", HomeTemplate: "%d/%n", DefaultVolatileDir: "/vol/%u", DefaultIndexDir: "/idx/%u",
		DefaultControlDir: "/ctl/%u", DefaultAltDir: "/alt/%u", DefaultMailPath: "/mail/%u",
		DefaultSeparator: "/", DefaultStorageEscapeChar: "%", DefaultSkipNFCNormalize: true,
	}
	if !reflect.DeepEqual(*r, want) {
		t.Errorf("resolver = %+v\nwant       %+v", *r, want)
	}
}

// Each driver name builds its own store, the same one by either entry point.
func TestBuildMailboxByDriver(t *testing.T) {
	cfg := config.Defaults()
	info := &mailbox.UserInfo{Username: "u@x", Home: t.TempDir()}
	storeOf := func(b mailbox.MailboxBackend) string {
		return fmt.Sprintf("%T", mailbox.Driver(b.OpenUser(info)))
	}
	for driver, pkg := range map[string]string{"maildir": "maildir.", "sdbox": "dboxv2.", "mdbox": "mdbox."} {
		if got := storeOf(BuildMailboxByDriver(driver, cfg.Storage, nil)); !strings.Contains(got, pkg) {
			t.Errorf("BuildMailboxByDriver(%s) opens a %s", driver, got)
		}
		sc := cfg.Storage
		sc.MailDriver = driver
		if got := storeOf(BuildMailbox(sc, nil)); !strings.Contains(got, pkg) {
			t.Errorf("BuildMailbox with mail_driver %s opens a %s", driver, got)
		}
	}
}
