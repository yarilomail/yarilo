package maildir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A removal handed a name from before a flag write still takes the file: the
// name it wears now is what the listing says (#1797).
func TestARemovalAfterAFlagWriteInTheSameSecondStillUnlinks(t *testing.T) {
	box, _, _ := recSetup(t)
	const body = "From: a@b\r\nSubject: x\r\n\r\nbody\r\n"
	name, _, _, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if _, aerr := box.AssignUID("INBOX", name, 1); aerr != nil {
		t.Fatal(aerr)
	}
	// Warm the listing under the name the file wears now.
	if _, _, cerr := box.currentName("INBOX", maildirBase(name)); cerr != nil {
		t.Fatal(cerr)
	}
	renamed, werr := box.WriteFlags("INBOX", name, []string{`\Deleted`}, nil)
	if werr != nil {
		t.Fatal(werr)
	}
	if renamed == name {
		t.Fatal("the flag write did not rename the file, so this row proves nothing")
	}

	was := testutil.ToFloat64(metricRemoveMiss)
	// The name a reader took before the rename, which is what an expunge holds.
	if err := box.Remove("INBOX", name); err != nil {
		t.Fatalf("remove: %v", err)
	}
	cur := filepath.Join(box.folderPath("INBOX"), "cur")
	entries, rerr := os.ReadDir(cur)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 0 {
		t.Errorf("cur/ still holds %d files after the removal", len(entries))
	}
	if got := testutil.ToFloat64(metricRemoveMiss); got != was {
		t.Errorf("the removal counted a miss (%v, was %v); it found the file", got, was)
	}
}

// Two removals of one message: the second finds nothing under either name, and
// that is what the counter is for -- no error, because it is already gone.
func TestASecondRemovalOfOneMessageIsCountedNotFailed(t *testing.T) {
	box, _, _ := recSetup(t)
	const body = "From: a@b\r\nSubject: x\r\n\r\nbody\r\n"
	name, _, _, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if _, aerr := box.AssignUID("INBOX", name, 1); aerr != nil {
		t.Fatal(aerr)
	}
	if err := box.Remove("INBOX", name); err != nil {
		t.Fatal(err)
	}

	was := testutil.ToFloat64(metricRemoveMiss)
	if err := box.Remove("INBOX", name); err != nil {
		t.Errorf("the second removal failed instead of counting: %v", err)
	}
	if got := testutil.ToFloat64(metricRemoveMiss); got != was+1 {
		t.Errorf("the miss counter is %v, want one more than %v", got, was)
	}
}

// The rule itself: a directory touched inside the settle window says nothing
// about its own contents.
func TestAFreshlyTouchedDirectoryIsNotSettled(t *testing.T) {
	if settled(time.Now()) {
		t.Error("a directory touched now was taken as settled")
	}
	if !settled(time.Now().Add(-2 * mailbox.StaleTemp)) {
		t.Error("an old directory was not taken as settled")
	}
}
