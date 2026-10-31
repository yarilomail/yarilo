package mailops

import (
	"errors"
	"strings"
	"testing"
)

// failingACL refuses every change to its files.
type failingACL struct{}

func (failingACL) Rename(string, string) error { return errors.New("acl store refused") }
func (failingACL) Remove(string) error         { return errors.New("acl store refused") }

// A rename or delete that lands keeps its result when the ACL file cannot
// follow, and the miss is logged; renaming a missing folder fails first.
func TestAnACLThatCannotFollowDoesNotUndoTheFolderChange(t *testing.T) {
	o, box, _, _ := folderOps(t, 0)
	if err := o.CreateFolder(box, "Work", "Work"); err != nil {
		t.Fatal(err)
	}
	log := logged(t)
	if err := o.RenameFolder(box, failingACL{}, "Work", "Archive", "Work", "Archive"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := o.DeleteFolder(box, failingACL{}, "Archive", "Archive"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n := strings.Count(log.String(), "acl"); n < 2 {
		t.Errorf("logged %d ACL misses, want both", n)
	}
	if err := o.RenameFolder(box, failingACL{}, "Nowhere", "Else", "Nowhere", "Else"); err == nil {
		t.Error("renamed a folder that is not there")
	}
}
