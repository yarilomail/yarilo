package mailboxbuild

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// With the shipped defaults no dbox folder may be made inside the FTS index
// directory: a store converted from the reference has one in its INBOX (#2254).
func TestNoFolderIsMadeInsideTheFTSIndexDirectory(t *testing.T) {
	for _, driver := range []string{"mdbox", "sdbox"} {
		t.Run(driver, func(t *testing.T) {
			home := t.TempDir()
			u := ByDriver(driver, config.Defaults().Storage, nil).OpenUser(&mailbox.UserInfo{
				Username: "u@d.test", Home: home, Driver: driver, Separator: "/",
			})
			t.Cleanup(func() { _ = u.Close() })
			if err := u.Init(); err != nil {
				t.Fatal(err)
			}
			if err := u.Create("INBOX/fts-flatcurve"); err == nil {
				t.Error("Create(INBOX/fts-flatcurve) was accepted")
			}
			_ = filepath.WalkDir(home, func(p string, d os.DirEntry, err error) error {
				if err == nil && d.Name() == mailbox.FTSIndexDir {
					t.Errorf("%s exists on disk after a refused create", p)
				}
				return nil
			})
		})
	}
}
