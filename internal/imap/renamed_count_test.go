package imap

import (
	"errors"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The writer skips a message it cannot name, so the results are shorter than
// the writes and position lines up with nothing (#1724).
func TestRenamedCountsByUIDNotPosition(t *testing.T) {
	for _, tc := range []struct {
		name    string
		writes  []mailbox.FlagWrite
		results []mailbox.FlagWriteResult
		want    int
	}{
		{
			name:    "an unnamed message before a renamed one",
			writes:  []mailbox.FlagWrite{{UID: 1}, {UID: 2, Filename: "u.2:2,"}},
			results: []mailbox.FlagWriteResult{{UID: 2, Filename: "u.2:2,S"}},
			want:    1,
		},
		{
			name:    "a name that did not move",
			writes:  []mailbox.FlagWrite{{UID: 1}, {UID: 2, Filename: "u.2:2,S"}},
			results: []mailbox.FlagWriteResult{{UID: 2, Filename: "u.2:2,S"}},
			want:    0,
		},
		{
			name:    "a write that failed names nothing new",
			writes:  []mailbox.FlagWrite{{UID: 2, Filename: "u.2:2,"}},
			results: []mailbox.FlagWriteResult{{UID: 2, Filename: "u.2:2,S", Err: errors.New("no")}},
			want:    0,
		},
		{
			name:    "a uid the caller never sent",
			writes:  []mailbox.FlagWrite{{UID: 2, Filename: "u.2:2,"}},
			results: []mailbox.FlagWriteResult{{UID: 7, Filename: "u.7:2,S"}},
			want:    0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := renamedCount(tc.writes, tc.results); got != tc.want {
				t.Errorf("counted %d renames, want %d", got, tc.want)
			}
		})
	}
}
