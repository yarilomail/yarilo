package maildir

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// storedName is what a record is called on disk now: the field is gone, so a
// test asks the driver the way every caller does (#1700).
func storedName(t *testing.T, box mailbox.UserMailbox, folder string, m *mailbox.MessageMeta) string {
	t.Helper()
	name, err := mailbox.MessagePath(box, folder, m)
	if err != nil {
		t.Fatalf("uid %d has no name on disk: %v", m.UID, err)
	}
	return name
}
