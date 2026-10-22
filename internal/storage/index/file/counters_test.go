package file

import (
	"reflect"
	"testing"

	"github.com/yarilomail/yarilo/internal/userstate/counters"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Each visible write moves exactly the types a client would refetch; a write a
// client cannot see moves none, or every cache refresh would wake push streams.
func TestVisibleWritesMoveTheirTypes(t *testing.T) {
	root := t.TempDir()
	user := "alice@example.com"
	ui := openIdx(root, user)
	path := counters.Path(counters.Dir(ui.home, ui.controlRoot))
	var f *mailbox.Folder

	flagsOnly := []counters.Type{counters.Email, counters.Mailbox}
	mail := []counters.Type{counters.Email, counters.Mailbox, counters.Quota, counters.Thread}
	steps := []struct {
		name  string
		write func() error
		want  []counters.Type
	}{
		{"create folder", func() (err error) { f, err = ui.CreateFolder("Work", 42, ""); return err }, []counters.Type{counters.Mailbox}},
		{"append", func() error { return ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Size: 10}) }, mail},
		{"add flags", func() error { return ui.AddFlags(f.ID, 1, []string{`\Seen`}, nil) }, flagsOnly},
		{"tx flags only", func() error {
			tx, err := ui.Begin(f.ID)
			if err != nil {
				return err
			}
			tx.UpdateFlags(1, mailbox.FlagsUpdate{Mode: mailbox.FlagsAdd, Flags: []string{`\Flagged`}})
			_, err = tx.Commit()
			return err
		}, flagsOnly},
		{"tx append", func() error {
			tx, err := ui.Begin(f.ID)
			if err != nil {
				return err
			}
			tx.Append(&mailbox.MessageMeta{UID: 2, Size: 10})
			_, err = tx.Commit()
			return err
		}, mail},
		{"cache generation", func() error { _, err := ui.BumpCacheGeneration(f.ID); return err }, nil},
		{"expunge", func() error { return ui.ExpungeMessage(f.ID, 1) }, mail},
		{"rename folder", func() error { return ui.RenameFolder("Work", "Old") }, []counters.Type{counters.Mailbox}},
		{"delete folder", func() error { return ui.DeleteFolder("Old") }, mail},
	}
	for _, st := range steps {
		before, err := counters.Read(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.write(); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		after, err := counters.Read(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := after.ChangedAfter(before.Highest); !reflect.DeepEqual(got, st.want) {
			t.Errorf("%s moved %v, want %v", st.name, got, st.want)
		}
	}
}
