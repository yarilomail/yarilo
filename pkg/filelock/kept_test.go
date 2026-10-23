package filelock

import (
	"path/filepath"
	"testing"
	"time"
)

// A kept lock opens its file once, is taken and given back many times, and
// still orders this process's own takers.
func TestKeptLockReusesItsFile(t *testing.T) {
	k := Keep(filepath.Join(t.TempDir(), "x.lock"), MethodFcntl)
	defer k.Close() //nolint:errcheck
	unlock, err := k.Take(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	f := k.f
	if _, err := k.Take(50 * time.Millisecond); err == nil {
		t.Fatal("a second take in the same process succeeded while the first held")
	}
	unlock()
	for i := 0; i < 3; i++ {
		u, err := k.Take(time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if k.f != f {
			t.Fatal("the file was reopened between takes")
		}
		u()
	}
	if err := k.Close(); err != nil || k.f != nil {
		t.Fatalf("close: %v, file %v", err, k.f)
	}
}
