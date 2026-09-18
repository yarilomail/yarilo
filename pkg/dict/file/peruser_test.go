package file_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/yarilomail/yarilo/pkg/dict"
	_ "github.com/yarilomail/yarilo/pkg/dict/file"
)

func open(t *testing.T, path string) dict.Dict {
	t.Helper()
	d, err := dict.Open(dict.Config{Driver: "file", Settings: map[string]any{"path": path}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func set(t *testing.T, d dict.Dict, ops *dict.OpSettings, key, val string) {
	t.Helper()
	tx, err := d.Begin(context.Background(), ops)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Set(key, []byte(val)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// An annotation lands in the home of the user who set it, and the next session
// -- a fresh Dict, as a new process would have -- reads it back.
func TestAnAnnotationLandsInThatUsersHome(t *testing.T) {
	root := t.TempDir()
	one := &dict.OpSettings{Username: "u1@d.test", HomeDir: filepath.Join(root, "u1")}
	two := &dict.OpSettings{Username: "u2@d.test", HomeDir: filepath.Join(root, "u2")}

	d := open(t, "%h/yarilo-metadata.json")
	set(t, d, one, "/private/comment", "mine")

	if _, err := os.Stat(filepath.Join(root, "u1", "yarilo-metadata.json")); err != nil {
		t.Fatalf("the file is not in u1's home: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "u2", "yarilo-metadata.json")); !os.IsNotExist(err) {
		t.Errorf("u2's home holds a file after only u1 wrote: %v", err)
	}

	// The other user reads their own file, not u1's.
	if _, found, err := d.Lookup(context.Background(), two, "/private/comment"); err != nil || found {
		t.Errorf("u2 sees u1's annotation (found=%v, err=%v)", found, err)
	}

	next := open(t, "%h/yarilo-metadata.json")
	vals, found, err := next.Lookup(context.Background(), one, "/private/comment")
	if err != nil || !found {
		t.Fatalf("a new session did not read the annotation back (found=%v, err=%v)", found, err)
	}
	if string(vals[0]) != "mine" {
		t.Errorf("read back %q, want %q", vals[0], "mine")
	}
}

// A template naming the home with no home to put in it is an error, not a path
// every user shares.
func TestAPathNeedingAHomeRefusesWithoutOne(t *testing.T) {
	d := open(t, "%h/yarilo-metadata.json")
	if _, _, err := d.Lookup(context.Background(), &dict.OpSettings{Username: "u@d.test"}, "/k"); err == nil {
		t.Fatal("a lookup with no home was answered")
	}
}

// Two processes writing annotations at once: every write survives and the file
// is whole. Processes, not goroutines -- an in-process mutex passes this row
// without the file lock, and the file lock is what the deployment needs.
func TestTwoProcessesWritingKeepEveryAnnotation(t *testing.T) {
	if os.Getenv("YARILO_DICT_WRITER") != "" {
		home := os.Getenv("YARILO_DICT_HOME")
		d, err := dict.Open(dict.Config{Driver: "file", Settings: map[string]any{"path": "%h/yarilo-metadata.json"}})
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close() //nolint:errcheck
		ops := &dict.OpSettings{Username: "u1@d.test", HomeDir: home}
		for i := 0; i < 25; i++ {
			set(t, d, ops, "/private/"+os.Getenv("YARILO_DICT_WRITER")+"/"+strconv.Itoa(i), "v")
		}
		return
	}

	home := t.TempDir()
	done := make(chan error, 2)
	for _, who := range []string{"a", "b"} {
		cmd := exec.Command(os.Args[0], "-test.run", "TestTwoProcessesWritingKeepEveryAnnotation")
		cmd.Env = append(os.Environ(), "YARILO_DICT_WRITER="+who, "YARILO_DICT_HOME="+home)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() {
			done <- cmd.Wait()
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("writer failed: %v", err)
		}
	}

	d := open(t, "%h/yarilo-metadata.json")
	ops := &dict.OpSettings{Username: "u1@d.test", HomeDir: home}
	missing := 0
	for _, who := range []string{"a", "b"} {
		for i := 0; i < 25; i++ {
			if _, found, err := d.Lookup(context.Background(), ops, "/private/"+who+"/"+strconv.Itoa(i)); err != nil || !found {
				missing++
			}
		}
	}
	if missing > 0 {
		t.Errorf("%d of 50 annotations were lost: one writer overwrote the other's file", missing)
	}
}
