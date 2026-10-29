package maildir

import (
	"fmt"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
)

// A list with rows that are not rows, or a header that is not one, loses no
// file: after the reconcile every file is in the folder once, in uid order.
func TestAListWithRowsThatAreNotRowsLosesNoFile(t *testing.T) {
	files := []string{"1700000001.M1P1.host", "1700000002.M2P2.host", "1700000003.M3P3.host"}
	for name, rows := range map[string][]string{
		"a row with no name":        {"1 :" + files[0] + ":2,", "2 W40", "3 :" + files[2] + ":2,"},
		"a row with no uid":         {"1 :" + files[0] + ":2,", " :" + files[1] + ":2,", "3 :" + files[2] + ":2,"},
		"a row whose uid is a word": {"1 :" + files[0] + ":2,", "two :" + files[1] + ":2,", "3 :" + files[2] + ":2,"},
	} {
		t.Run(name, func(t *testing.T) {
			box, idx, folder := recSetup(t)
			for _, f := range files {
				deliverToCur(t, box, f+":2,", "From: a@b\r\n\r\n"+f+"\r\n")
			}
			writeList(t, box, fmt.Sprintf("3 V%d N4 G00000000000000000000000000000000", folder.UIDValidity), rows...)
			if _, err := box.ReconcileIndex(mailboxbase.Open(box, idx), idx, folder); err != nil {
				t.Fatalf("the reconcile did not finish: %v", err)
			}
			if got := folderUIDs(t, idx, folder); len(got) != len(files) || !ascending(got) {
				t.Errorf("the folder holds uids %v, want all %d files once", got, len(files))
			}
		})
	}
	t.Run("a header from another version", func(t *testing.T) {
		box, idx, folder := recSetup(t)
		for _, f := range files {
			deliverToCur(t, box, f+":2,", "From: a@b\r\n\r\n"+f+"\r\n")
		}
		writeList(t, box, "1 4 7", "1 :"+files[0]+":2,")
		if v, _, ok := box.UIDSpace("INBOX"); ok {
			t.Errorf("a version-1 header read as uid space %d", v)
		}
		if _, err := box.ReconcileIndex(mailboxbase.Open(box, idx), idx, folder); err != nil {
			t.Fatalf("the reconcile did not finish: %v", err)
		}
		if got := folderUIDs(t, idx, folder); len(got) != len(files) {
			t.Errorf("the folder holds uids %v, want all %d files", got, len(files))
		}
	})
}
