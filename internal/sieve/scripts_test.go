package sieve

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const ownScript = "require [\"fileinto\"];\nfileinto \"Lists\";\n"

// switchers are the two commands that unlink the active path.
var switchers = []struct {
	name string
	run  func(*FsScriptStore, string) error
}{
	{"deactivate", func(ss *FsScriptStore, home string) error {
		return ss.Deactivate(context.Background(), "u", home)
	}},
	{"activate", func(ss *FsScriptStore, home string) error {
		if err := ss.SaveScript(context.Background(), "u", home, "next", []byte("keep;\n")); err != nil {
			return err
		}
		return ss.SetActive(context.Background(), "u", home, "next")
	}},
}

func storeWithActive(t *testing.T, body string) (*FsScriptStore, string) {
	t.Helper()
	ss := &FsScriptStore{DefaultName: FallbackDefaultName}
	home := t.TempDir()
	if err := os.WriteFile(ss.activePath(home), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return ss, home
}

func TestAnActiveFileOfItsOwnIsKeptAsOrig(t *testing.T) {
	for _, sw := range switchers {
		t.Run(sw.name, func(t *testing.T) {
			ss, home := storeWithActive(t, ownScript)
			if err := sw.run(ss, home); err != nil {
				t.Fatal(err)
			}
			got, ok, err := ss.GetScript(context.Background(), "u", home, OrigScriptName)
			if err != nil || !ok {
				t.Fatalf("no %s script after %s: ok=%v err=%v", OrigScriptName, sw.name, ok, err)
			}
			if !bytes.Equal(got, []byte(ownScript)) {
				t.Errorf("%s holds %q, want the active file's bytes %q", OrigScriptName, got, ownScript)
			}
			names, _ := ss.ListScripts(context.Background(), "u", home)
			if !strings.Contains(strings.Join(names, ","), OrigScriptName) {
				t.Errorf("LISTSCRIPTS %v does not show %s", names, OrigScriptName)
			}
		})
	}
}

func TestTheDefaultActiveFileLeavesNoCopy(t *testing.T) {
	for _, sw := range switchers {
		t.Run(sw.name, func(t *testing.T) {
			ss, home := storeWithActive(t, DefaultScriptBody)
			if err := sw.run(ss, home); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(ss.namedPath(home, OrigScriptName)); !os.IsNotExist(err) {
				t.Errorf("the default body was kept as %s: %v", OrigScriptName, err)
			}
		})
	}
}

func TestAnActiveLinkIsOnlyUnlinked(t *testing.T) {
	for _, sw := range switchers {
		t.Run(sw.name, func(t *testing.T) {
			ss := &FsScriptStore{DefaultName: FallbackDefaultName}
			home := t.TempDir()
			ctx := context.Background()
			if err := ss.SaveScript(ctx, "u", home, "mine", []byte(ownScript)); err != nil {
				t.Fatal(err)
			}
			if err := ss.SetActive(ctx, "u", home, "mine"); err != nil {
				t.Fatal(err)
			}
			if err := sw.run(ss, home); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(ss.namedPath(home, OrigScriptName)); !os.IsNotExist(err) {
				t.Errorf("a link's target was copied to %s: %v", OrigScriptName, err)
			}
			if got, ok, _ := ss.GetScript(ctx, "u", home, "mine"); !ok || !bytes.Equal(got, []byte(ownScript)) {
				t.Errorf("the linked script changed: ok=%v %q", ok, got)
			}
		})
	}
}

func TestAnActivePathThatIsNeitherIsRefused(t *testing.T) {
	for _, sw := range switchers {
		t.Run(sw.name, func(t *testing.T) {
			ss := &FsScriptStore{DefaultName: FallbackDefaultName}
			home := t.TempDir()
			path := ss.activePath(home)
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			err := sw.run(ss, home)
			if err == nil || !strings.Contains(err.Error(), filepath.Base(path)) || !strings.Contains(err.Error(), "neither") {
				t.Fatalf("%s on a directory: err %v, want one naming %s", sw.name, err, path)
			}
			if fi, err := os.Lstat(path); err != nil || !fi.IsDir() {
				t.Errorf("the directory did not survive: %v", err)
			}
		})
	}
}
