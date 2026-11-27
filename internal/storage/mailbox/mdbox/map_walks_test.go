package mdbox

import (
	"io"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/mdbox/mdboxmap"
)

// A delivery, a fetch and an expunge on a session that opened a folded map walk
// none of its records, and a delivery reads none of the base (#2642).
func TestSessionPathsWalkNoMapRecords(t *testing.T) {
	home := t.TempDir()
	seed := openTestUserMailbox(t, home)
	var names []string
	for i := 0; i < 300; i++ {
		name, _, _, err := seed.Save("INBOX", strings.NewReader("Subject: s\r\n\r\nbody\r\n"), 0, 0, nil, nil, [16]byte{})
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	m, err := seed.openMap()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Compact(); err != nil {
		t.Fatal(err)
	}
	_ = seed.Close()

	u := openTestUserMailbox(t, home)
	defer u.Close() //nolint:errcheck
	tests := []struct {
		name      string
		op        func() error
		readsBase bool
	}{
		{"delivery", func() error {
			_, _, _, err := u.Save("INBOX", strings.NewReader("Subject: n\r\n\r\nnew\r\n"), 0, 0, nil, nil, [16]byte{})
			return err
		}, false},
		{"fetch", func() error {
			rc, err := u.Fetch("INBOX", names[150], false)
			if err != nil {
				return err
			}
			defer rc.Close() //nolint:errcheck
			_, err = io.Copy(io.Discard, rc)
			return err
		}, true},
		{"expunge", func() error { return u.RemoveManyHeld("INBOX", names[10:20]) }, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mdboxmap.ResetCounts()
			if err := tc.op(); err != nil {
				t.Fatal(err)
			}
			if n := mdboxmap.FullScans(); n != 0 {
				t.Errorf("%s walked the whole map %d times", tc.name, n)
			}
			if n := mdboxmap.BaseReads(); (n != 0) != tc.readsBase {
				t.Errorf("%s read the base %d times, want reads %v", tc.name, n, tc.readsBase)
			}
		})
	}
}
