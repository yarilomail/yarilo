//go:build flatcurve

package ftsservice

import (
	"sort"
	"strings"
	"testing"
)

// A whole-user rescan holds the user's index once for every folder: a hold per
// folder made the command queue behind its own queued job (#1986).
func TestWholeUserRescanTakesOneHold(t *testing.T) {
	// No messages: a folder with a gap queues an index job, and that job takes
	// the user's index too -- which would be counted as the command's hold.
	svc, box, _ := newTestService(t)
	for _, name := range []string{"Archive", "Sent"} {
		if err := box.Create(name); err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
	}

	var holds int
	svc.opts.LockMailbox = func(_, folder string, fn func() error) error {
		holds++
		if folder != "" {
			t.Errorf("the hold was keyed on folder %q, but the index is the user's", folder)
		}
		return fn()
	}

	done, err := svc.RescanUser(testUser)
	if err != nil {
		t.Fatalf("RescanUser: %v", err)
	}
	sort.Strings(done)
	if want := "Archive,INBOX,Sent"; strings.Join(done, ",") != want {
		t.Errorf("rescanned %v, want %s", done, want)
	}
	if holds != 1 {
		t.Errorf("the command took %d holds for %d folders, want 1", holds, len(done))
	}
}

// The folders come from the mailbox, so one that holds no index yet is still
// visited: that is the folder a rescan exists to reconcile.
func TestWholeUserRescanVisitsAFolderWithNoIndex(t *testing.T) {
	svc, box, uidx := newTestService(t)
	saveMessage(t, box, uidx, 1, "alpha")
	if err := box.Create("Fresh"); err != nil {
		t.Fatal(err)
	}
	done, err := svc.RescanUser(testUser)
	if err != nil {
		t.Fatalf("RescanUser: %v", err)
	}
	if !contains(done, "Fresh") {
		t.Errorf("rescanned %v, which leaves out the folder that has no index", done)
	}
}

func contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}
