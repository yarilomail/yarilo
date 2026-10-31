//go:build flatcurve

package ftsservice

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// fts_autoindex_max_recent_msgs skips the autoindex pass of a folder holding
// more \Recent messages than the limit, counted from the same mark as IMAP's;
// 0 is no limit.
func TestAutoindexHonoursMaxRecent(t *testing.T) {
	cases := []struct {
		name    string
		limit   int
		claimed bool // a session selected the folder after delivery
		indexed bool
	}{
		{"no limit", 0, false, true},
		{"recent within the limit", 3, false, true},
		{"recent above the limit", 2, false, false},
		{"claimed by a SELECT, nothing recent", 2, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, box, uidx := newTestService(t)
			for uid := uint32(1); uid <= 3; uid++ {
				saveMessage(t, box, uidx, uid, "word")
			}
			if tc.claimed {
				f, err := uidx.OpenFolder(testMbox.Name, testMbox.UIDValidity)
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := uidx.(mailbox.RecentClaimer).ClaimRecent(f.ID); err != nil {
					t.Fatal(err)
				}
			}
			skipped := testutil.ToFloat64(metricAutoindexSkippedRecent)
			if err := svc.Index(testUser, testMbox, 3, tc.limit); err != nil {
				t.Fatal(err)
			}
			if tc.indexed {
				waitIndexed(t, svc, 3)
				return
			}
			deadline := time.Now().Add(10 * time.Second)
			for testutil.ToFloat64(metricAutoindexSkippedRecent) == skipped {
				if time.Now().After(deadline) {
					t.Fatal("the pass was neither skipped nor counted")
				}
				time.Sleep(20 * time.Millisecond)
			}
			if last, _, err := svc.Status(testUser, testMbox); err != nil || last != 0 {
				t.Errorf("checkpoint %d (%v) after a skipped pass, want 0", last, err)
			}
		})
	}
}
