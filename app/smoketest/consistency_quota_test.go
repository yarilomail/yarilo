package main

import (
	"strings"
	"testing"
)

// The quota row against a stub: the two surfaces report the same account's
// usage, and a divergence is what neither of them can see alone, because each
// is self-consistent.
func TestQuotaRowJudgement(t *testing.T) {
	imapSide := func(used, limit string) *reading {
		return newReading(surfIMAP).field("storageUsedKiB", used).field("storageLimitKiB", limit)
	}
	adminSide := func(used, limit string) *reading {
		return newReading(surfAdminAPI).field("storageUsedKiB", used).field("storageLimitKiB", limit)
	}

	tests := []struct {
		name   string
		left   *reading
		right  *reading
		wantIn string
	}{
		{"the same numbers", imapSide("2048", "10240"), adminSide("2048", "10240"), ""},
		{"a different usage", imapSide("2048", "10240"), adminSide("4096", "10240"), "storageUsedKiB"},
		{"a different limit", imapSide("2048", "10240"), adminSide("2048", "20480"), "storageLimitKiB"},
		// Each reader spells no limit as unlimited; IMAP reports no usage then.
		{"unlimited on both", newReading(surfIMAP).field("storageLimitKiB", unlimited), adminSide("2048", unlimited), ""},
		{"unlimited against a limit", newReading(surfIMAP).field("storageLimitKiB", unlimited), adminSide("2048", "10240"), "storageLimitKiB"},
		{"a limit against unlimited", imapSide("2048", "10240"), adminSide("2048", unlimited), "storageLimitKiB"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := judgeRow("quota", tc.left, tc.right, defaultAllowances())
			if tc.wantIn == "" {
				if err != nil {
					t.Fatalf("agreeing quota numbers were refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("diverging quota numbers were accepted")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("verdict %q does not name %q", err, tc.wantIn)
			}
		})
	}
}

// The IMAP QUOTA response is scraped, so the scraping is pinned: a parser that
// silently returns zeros would make every quota row agree with an admin API
// that also reported zero for an account it could not read.
func TestParseQuotaStorage(t *testing.T) {
	tests := []struct {
		name              string
		lines             []string
		wantUsed, wantLim int64
		wantOK            bool
	}{
		{
			name:     "storage alone",
			lines:    []string{`* QUOTAROOT "INBOX" "User quota"`, `* QUOTA "User quota" (STORAGE 2048 10240)`},
			wantUsed: 2048, wantLim: 10240, wantOK: true,
		},
		{
			name:     "storage beside another resource",
			lines:    []string{`* QUOTA "User quota" (MESSAGE 12 100 STORAGE 2048 10240)`},
			wantUsed: 2048, wantLim: 10240, wantOK: true,
		},
		{
			name:  "no storage resource at all",
			lines: []string{`* QUOTA "User quota" (MESSAGE 12 100)`},
		},
		{
			name:  "a response carrying no quota",
			lines: []string{`* CAPABILITY IMAP4rev1 QUOTA`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			used, limit, ok := parseQuotaStorage(tc.lines)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && (used != tc.wantUsed || limit != tc.wantLim) {
				t.Errorf("(used, limit) = (%d, %d), want (%d, %d)", used, limit, tc.wantUsed, tc.wantLim)
			}
		})
	}
}

// Every surface spells a quota without a limit as unlimited: IMAP by a root
// that lists no STORAGE, the admin API by -1, JMAP by no octets root (#2455).
func TestQuotaReadingsSpellUnlimitedAlike(t *testing.T) {
	tests := []struct {
		name  string
		got   func() (*reading, error)
		limit string
	}{
		{"imap with a limit", func() (*reading, error) {
			return imapQuotaReading([]string{`* QUOTA "User quota" (STORAGE 23 100)`})
		}, "100"},
		{"imap without one", func() (*reading, error) {
			return imapQuotaReading([]string{`* QUOTAROOT INBOX "User quota"`, `* QUOTA "User quota" ()`})
		}, unlimited},
		{"imap with only a message limit", func() (*reading, error) {
			return imapQuotaReading([]string{`* QUOTA "User quota" (MESSAGE 55 1000)`})
		}, unlimited},
		{"admin API with a limit", func() (*reading, error) { return adminQuotaReading(23, 100), nil }, "100"},
		{"admin API without one", func() (*reading, error) { return adminQuotaReading(23, -1), nil }, unlimited},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := tc.got()
			if err != nil {
				t.Fatal(err)
			}
			if got := r.fields["storageLimitKiB"]; got != tc.limit {
				t.Errorf("storageLimitKiB = %q, want %q", got, tc.limit)
			}
		})
	}
	if _, err := imapQuotaReading([]string{`* CAPABILITY IMAP4rev1 QUOTA`}); err == nil {
		t.Error("a response with no QUOTA line read as a quota")
	}
}
