package maildir

import (
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// The list's stamp after a save's row is handed to the index only when the
// list was in the state the index agreed with and grew by that row alone;
// otherwise the old stamp stays and the next save reads the list (#2643).
func TestOnlyTheRowAloneMovesTheListStamp(t *testing.T) {
	agreed := mailbox.MaildirStamp{UIDListMtime: 10, UIDListSize: 100}
	cases := []struct {
		name   string
		before mailbox.MaildirStamp
		after  uint32
		want   bool
	}{
		{name: "the row alone", before: agreed, after: 160, want: true},
		{name: "another writer's bytes too", before: agreed, after: 200, want: false},
		{name: "a list the index did not agree with", before: mailbox.MaildirStamp{UIDListMtime: 11, UIDListSize: 100}, after: 160, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &userMailbox{}
			u.agreeList("INBOX", agreed)
			u.appendedFrom("INBOX", tc.before, mailbox.MaildirStamp{UIDListMtime: 12, UIDListSize: tc.after}, 60)
			if _, got := u.AppendedListStamp("INBOX"); got != tc.want {
				t.Errorf("handed a stamp %v, want %v", got, tc.want)
			}
		})
	}
}
