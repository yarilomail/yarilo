package imap_test

import (
	"slices"
	"testing"

	imaplib "github.com/emersion/go-imap/v2"
)

// A body FETCH sets \Seen only for a reader holding the s right; without it
// the body is still served.
func TestFetchSetsSeenOnlyWithTheSeenRight(t *testing.T) {
	for _, tc := range []struct {
		rights   string
		wantSeen bool
	}{
		{"lr", false},
		{"lrs", true},
	} {
		t.Run(tc.rights, func(t *testing.T) {
			aliceHome, dial := enforceServerWithShared(t)
			a := dial("alice")
			body := []byte("From: x@y\r\nSubject: t\r\n\r\nbody\r\n")
			ac := a.Append("INBOX", int64(len(body)), nil)
			_, _ = ac.Write(body)
			_ = ac.Close()
			if _, err := ac.Wait(); err != nil {
				t.Fatalf("alice APPEND: %v", err)
			}
			seedACL(t, aliceHome, "INBOX", "user=bob "+tc.rights+"\n")

			b := dial("bob")
			if _, err := b.Select("Shared/INBOX", nil).Wait(); err != nil {
				t.Fatalf("bob SELECT: %v", err)
			}
			msgs, err := b.Fetch(imaplib.SeqSetNum(1), &imaplib.FetchOptions{
				BodySection: []*imaplib.FetchItemBodySection{{}},
			}).Collect()
			if err != nil || len(msgs) != 1 || len(msgs[0].BodySection) == 0 {
				t.Fatalf("bob FETCH BODY[] = %v, %v; want the body", msgs, err)
			}

			if _, err := a.Select("INBOX", nil).Wait(); err != nil {
				t.Fatalf("alice SELECT: %v", err)
			}
			flags, err := a.Fetch(imaplib.SeqSetNum(1), &imaplib.FetchOptions{Flags: true}).Collect()
			if err != nil || len(flags) != 1 {
				t.Fatalf("alice FETCH FLAGS: %v, %v", flags, err)
			}
			if got := slices.Contains(flags[0].Flags, imaplib.FlagSeen); got != tc.wantSeen {
				t.Fatalf("\\Seen = %v after bob's FETCH with %q, want %v", got, tc.rights, tc.wantSeen)
			}
		})
	}
}
