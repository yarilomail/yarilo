package maildir

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	fileidx "github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// Two writes of ours in two processes overlap: the stamp ends at the
// directory's time, or stays behind it and the next open walks; it never says
// settled for a time no walk and no carry of ours saw (#2661).
func TestTwoCarriesThatOverlapNeverSettleAnUnseenTime(t *testing.T) {
	type step int
	const (
		aBefore step = iota
		bBefore
		aWrite
		bWrite
		aAfter
		bAfter
		aCarry
		bCarry
	)
	tests := []struct {
		name    string
		order   []step
		settled bool // every change is ours and carried, so the stamp may end at the directory's time
	}{
		{"both read before either writes", []step{aBefore, bBefore, aWrite, aAfter, bWrite, bAfter, aCarry, bCarry}, false},
		{"b starts after a's write", []step{aBefore, aWrite, aAfter, bBefore, bWrite, bAfter, aCarry, bCarry}, true},
		{"b carries first", []step{aBefore, aWrite, aAfter, bBefore, bWrite, bAfter, bCarry, aCarry}, false},
		{"b writes and never carries", []step{aBefore, aWrite, aAfter, bBefore, bWrite, aCarry}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := testHome(t.TempDir(), "u@x.com")
			box, idx := openWindowUser(t, home)
			if err := box.Init(); err != nil {
				t.Fatal(err)
			}
			if err := box.Create("INBOX"); err != nil {
				t.Fatal(err)
			}
			f, err := idx.OpenFolder("INBOX", 1)
			if err != nil {
				t.Fatal(err)
			}
			cur := filepath.Join(box.folderPath("INBOX"), "cur")
			at := time.Now().Add(-time.Hour)
			touch := func(sec int64) {
				if err := os.Chtimes(cur, at, at.Add(time.Duration(sec)*time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			touch(0)
			if _, err := box.ReconcileIndex(mailboxbase.Open(box, idx), idx, f); err != nil {
				t.Fatal(err)
			}
			other := fileidx.New().OpenUser(&mailbox.UserInfo{Username: "u@x.com", Home: home})
			t.Cleanup(func() { other.Close() }) //nolint:errcheck
			fo, err := other.OpenFolder("INBOX", f.UIDValidity)
			if err != nil {
				t.Fatal(err)
			}
			var aPre, bPre, aPost, bPost mailbox.DirTimes
			for _, s := range tc.order {
				switch s {
				case aBefore:
					aPre = box.DirTimes("INBOX")
				case bBefore:
					bPre = box.DirTimes("INBOX")
				case aWrite:
					touch(10)
				case bWrite:
					touch(20)
				case aAfter:
					aPost = box.DirTimes("INBOX")
				case bAfter:
					bPost = box.DirTimes("INBOX")
				case aCarry:
					if err := idx.(mailbox.DirCarrier).CarryDirTimes(f.ID, aPre, aPost); err != nil {
						t.Fatal(err)
					}
				case bCarry:
					if err := other.(mailbox.DirCarrier).CarryDirTimes(fo.ID, bPre, bPost); err != nil {
						t.Fatal(err)
					}
				}
			}
			now := at.Add(time.Hour)
			if got, _ := box.SyncedPerIndex(idx, f.ID, "INBOX", now); got != tc.settled {
				st, _ := idx.(mailbox.MaildirStamped).MaildirStamp(f.ID)
				t.Fatalf("settled %v, want %v: stamp cur %d.%09d, directory %+v", got, tc.settled, st.CurMtime, st.CurMtimeNsecs, box.DirTimes("INBOX"))
			}
		})
	}
}
