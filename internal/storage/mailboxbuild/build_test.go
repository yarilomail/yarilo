package mailboxbuild

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func TestParseIntervalSeconds(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0}, {"0", 0}, {"30", 30}, {"45", 45},
		{"30s", 30}, {"5m", 300}, {"1h", 3600}, {"90m", 5400},
		{"-5", 0}, {"garbage", 0}, {"10x", 0},
	}
	for _, c := range cases {
		if got := ParseIntervalSeconds(c.in); got != c.want {
			t.Errorf("ParseIntervalSeconds(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// mail_alt_path reaches the mdbox a factory builds through the resolver's
// UserInfo.AltDir, so altmove works (#639); without it alt storage is off.
func TestMdboxAltStorageComesFromTheResolver(t *testing.T) {
	altEnabled := func(defaultAlt string) bool {
		r := mailbox.Resolver{Root: t.TempDir(), HomeTemplate: "%d/%n", DefaultAltDir: defaultAlt}
		ui, err := r.UserInfo("u@d.test", "")
		if err != nil {
			t.Fatal(err)
		}
		u := ByDriver("mdbox", config.StorageConfig{MailAltPath: defaultAlt}, nil).OpenUser(ui)
		// The factory hands back a validating wrapper, so the driver's own
		// optional capabilities are asserted underneath it (#1069).
		return mailbox.Driver(u).(interface{ AltEnabled() bool }).AltEnabled()
	}
	if !altEnabled("/mnt/cold/%{user | domain}/%n") {
		t.Error("mail_alt_path set but AltEnabled() is false: alt storage not threaded (#639)")
	}
	if altEnabled("") {
		t.Error("no alt path configured but AltEnabled() is true")
	}
}

// Every driver that can make a body durable is given the configured mode. The
// default hid a driver that was never wired: sdbox synced nothing and no row
// could tell, because a driver that syncs by default looks the same (#1969).
func TestEveryDriverIsGivenTheConfiguredFsyncMode(t *testing.T) {
	type fsyncer interface{ FsyncMode() mailbox.FsyncMode }
	for _, driver := range []string{"maildir", "mdbox", "sdbox", "dbox"} {
		t.Run(driver, func(t *testing.T) {
			for _, want := range []mailbox.FsyncMode{mailbox.FsyncNever, mailbox.FsyncAlways, mailbox.FsyncOptimized} {
				box := byDriver(driver, config.StorageConfig{MailFsync: string(want)}, nil)
				f, ok := box.(fsyncer)
				if !ok {
					t.Fatalf("%T names no fsync mode, so the configured one cannot be checked", box)
				}
				if got := f.FsyncMode(); got != want {
					t.Errorf("%s was built with %q, config says %q", driver, got, want)
				}
			}
		})
	}
}
