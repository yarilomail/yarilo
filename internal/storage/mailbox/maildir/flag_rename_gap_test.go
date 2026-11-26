package maildir

import (
	"os"
	"path/filepath"
	"testing"
)

// A file that leaves its name between the lookup and the flag rename is "not
// there", not an error: the write re-syncs and lands on the name it moved to,
// or finds the message gone (#2701).
func TestAFileMovedBeforeItsFlagRenameIsNotAnError(t *testing.T) {
	const base = "1700000003.M3P1.host,S=20,W=20:2,"
	rows := []struct {
		name string
		gap  func(t *testing.T, from string)
		// want is the file in cur/ after the write; "" means none of this message.
		want string
	}{
		{
			name: "another handle changed its flags",
			gap: func(t *testing.T, from string) {
				if err := os.Rename(from, from+"F"); err != nil {
					t.Fatal(err)
				}
			},
			want: "1700000003.M3P1.host,S=20,W=20:2,S",
		},
		{
			name: "expunged",
			gap: func(t *testing.T, from string) {
				if err := os.Remove(from); err != nil {
					t.Fatal(err)
				}
			},
			want: "",
		},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			u, home := item1Folder(t, 5)
			fired := false
			beforeFlagRenameCall = func(from string) {
				if fired || filepath.Base(from) != base {
					return
				}
				fired = true
				r.gap(t, from)
			}
			t.Cleanup(func() { beforeFlagRenameCall = nil })

			if _, err := u.writeFlagsLocked("INBOX", base, []string{`\Seen`}, ""); err != nil {
				t.Fatalf("a file moved under the rename came back as an error: %v", err)
			}
			if !fired {
				t.Fatal("the gap never ran, so the row asserts nothing")
			}
			got := namesOfBase(t, filepath.Join(home, "Maildir", "cur"), maildirBase(base))
			switch {
			case r.want == "" && len(got) != 0:
				t.Errorf("cur/ holds %v, want the message gone", got)
			case r.want != "" && (len(got) != 1 || got[0] != r.want):
				t.Errorf("cur/ holds %v, want [%s]", got, r.want)
			}
		})
	}
}

// Only a missing source is "not there": a rename the directory refuses is
// still the error it was.
func TestAFlagRenameTheDirectoryRefusesStaysAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root renames in a read-only directory")
	}
	const base = "1700000003.M3P1.host,S=20,W=20:2,"
	u, home := item1Folder(t, 5)
	cur := filepath.Join(home, "Maildir", "cur")
	if err := os.Chmod(cur, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cur, 0o700) })

	if _, err := u.writeFlagsLocked("INBOX", base, []string{`\Seen`}, ""); err == nil {
		t.Fatal("a refused rename was reported as done")
	}
}

func namesOfBase(t *testing.T, dir, base string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if maildirBase(e.Name()) == base {
			out = append(out, e.Name())
		}
	}
	return out
}
