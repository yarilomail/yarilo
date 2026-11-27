package filelock_test

import (
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/filelock"
)

func TestDefaultsAreTheEmptyLockMethod(t *testing.T) {
	empty, _ := filelock.Parse("")
	if got := config.Defaults().Storage.LockMethod; got != string(empty) {
		t.Fatalf("defaults %q, empty gave %q", got, empty)
	}
}
