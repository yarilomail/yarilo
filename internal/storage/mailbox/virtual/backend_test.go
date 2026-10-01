package virtual

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func openUser(t *testing.T, root string) mailbox.UserMailbox {
	t.Helper()
	return New().OpenUser(&mailbox.UserInfo{Username: "u@test", Home: root, MailPath: root, Separator: "/"})
}

func writeConfig(t *testing.T, dir, text string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A mailbox is a directory with a configuration file, at any depth; one
// without is not a mailbox, however it is named.
func TestListFoldersNamesTheOnesWithAConfiguration(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, filepath.Join(root, "All"), "*\n")
	writeConfig(t, filepath.Join(root, "Views", "Unseen"), "INBOX\n  unseen\n")
	if err := os.MkdirAll(filepath.Join(root, "NotOne"), 0o700); err != nil {
		t.Fatal(err)
	}

	u := openUser(t, root)
	folders, err := u.ListFolders()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range folders {
		names = append(names, f.Name)
	}
	if len(names) != 2 || names[0] != "All" || names[1] != "Views/Unseen" {
		t.Errorf("listed %v, want [All Views/Unseen]", names)
	}
	for _, f := range folders {
		if !f.Selectable {
			t.Errorf("%q is listed as not selectable, so LIST marks it \\Noselect", f.Name)
		}
	}
	if ok, _ := u.FolderExists("NotOne"); ok {
		t.Error("a directory with no configuration was called a mailbox")
	}
	if ok, _ := u.FolderExists("All"); !ok {
		t.Error("a directory with a configuration was not")
	}
}

// The foreign name is read when ours is absent, and never written.
func TestForeignConfigurationIsRead(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Legacy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, LegacyConfigFileName), []byte("INBOX\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	u := openUser(t, root)
	if ok, err := u.FolderExists("Legacy"); !ok || err != nil {
		t.Errorf("a foreign configuration was not read: ok=%v err=%v", ok, err)
	}
}

// A client cannot make one: the definition is the file, so a mailbox made
// without it would be a mailbox with no rule.
func TestCreateIsRefused(t *testing.T) {
	u := openUser(t, t.TempDir())
	err := u.Create("Whatever")
	if err == nil {
		t.Fatal("CREATE was accepted in a virtual namespace")
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("CREATE failed with %v, want the namespace's refusal", err)
	}
}

// Nothing is stored here: a virtual mailbox names messages that live elsewhere.
func TestStoringIsRefused(t *testing.T) {
	u := openUser(t, t.TempDir())
	if _, _, _, err := u.Save("All", nil, 1, 0, nil, nil, [16]byte{}); !errors.Is(err, ErrNotStored) {
		t.Errorf("Save answered %v", err)
	}
	if err := u.Remove("All", "x"); !errors.Is(err, ErrNotStored) {
		t.Errorf("Remove answered %v", err)
	}
	if _, err := u.Scan("All"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Scan answered %v, and a rebuild from storage would empty the mailbox", err)
	}
}

// The namespace is reached through links the filesystem resolves, not only
// through directories: a mounted ConfigMap presents each key that way.
func TestListFoldersResolvesLinksAndSkipsTheMountsBookkeeping(t *testing.T) {
	tests := []struct {
		name  string
		build func(t *testing.T, root string)
		want  []string
	}{
		{
			name: "the shape a mounted ConfigMap takes",
			build: func(t *testing.T, root string) {
				data := "..2026_10_01_07_45_29.2901286641"
				writeConfig(t, filepath.Join(root, data, "All"), "*\n")
				symlink(t, data, filepath.Join(root, "..data"))
				symlink(t, filepath.Join("..data", "All"), filepath.Join(root, "All"))
			},
			want: []string{"All"},
		},
		{
			name: "a link to a directory outside the namespace",
			build: func(t *testing.T, root string) {
				outside := t.TempDir()
				writeConfig(t, filepath.Join(outside, "Shared"), "*\n")
				symlink(t, filepath.Join(outside, "Shared"), filepath.Join(root, "Shared"))
			},
			want: []string{"Shared"},
		},
		{
			// Named before the mailbox on purpose: a listing that stops at the
			// unresolvable entry loses what comes after it.
			name: "a link that resolves to nothing",
			build: func(t *testing.T, root string) {
				symlink(t, "nowhere", filepath.Join(root, "Gone"))
				writeConfig(t, filepath.Join(root, "Mail"), "*\n")
			},
			want: []string{"Mail"},
		},
		{
			name: "a link back to the directory holding it",
			build: func(t *testing.T, root string) {
				writeConfig(t, filepath.Join(root, "All"), "*\n")
				symlink(t, ".", filepath.Join(root, "Loop"))
			},
			want: []string{"All"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			tt.build(t, root)

			folders, err := openUser(t, root).ListFolders()
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			var names []string
			for _, f := range folders {
				names = append(names, f.Name)
			}
			if len(names) != len(tt.want) {
				t.Fatalf("listed %v, want %v", names, tt.want)
			}
			for i, want := range tt.want {
				if names[i] != want {
					t.Errorf("listed %v, want %v", names, tt.want)
				}
			}
		})
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
