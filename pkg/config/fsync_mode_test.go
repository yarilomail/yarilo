package config

import (
	"strings"
	"testing"
)

// A typo in a durability or an exclusion setting refuses the start: silently
// serving under a default nobody asked for is how a deployment finds out it
// was not syncing on the day it loses a node (#1847).
func TestAnUnknownDurabilityOrLockSettingRefusesTheStart(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"mail_fsync", "mail_fsync: optimised", "mail_fsync"},
		{"lock method", "storage_lock_method: flok", "storage_lock_method"},
		{"cache purge above 100", "mail_cache_purge_delete_percentage: 101", "mail_cache_purge_delete_percentage"},
		{"cache purge -1, off is 0 now", "mail_cache_purge_delete_percentage: -1", "mail_cache_purge_delete_percentage"},
		{"cache purge below -1", "mail_cache_purge_delete_percentage: -2", "mail_cache_purge_delete_percentage"},
		{"continued share negative", "mail_cache_purge_continued_percentage: -1", "mail_cache_purge_continued_percentage"},
		{"cache purge size negative", "mail_cache_purge_min_size: \"-1k\"", "mail_cache_purge_min_size"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadStorage(t, tc.body)
			if err == nil {
				t.Fatalf("a config saying %q started", tc.body)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal is %q and does not name %q", err, tc.want)
			}
		})
	}
}

// And every value the reference defines is taken.
func TestEveryDurabilityModeIsAccepted(t *testing.T) {
	for _, mode := range []string{"never", "optimized", "always", ""} {
		body := "mail_fsync: " + mode
		if mode == "" {
			body = "mail_driver: maildir" // the key absent is the default
		}
		if _, err := loadStorage(t, body); err != nil {
			t.Errorf("mail_fsync %q was refused: %v", mode, err)
		}
	}
}

// The purge shares and the minimum size take every value the reference does, and
// the absent keys load as unset.
func TestEveryCachePurgeShareIsAccepted(t *testing.T) {
	for _, body := range []string{"mail_cache_purge_delete_percentage: 0", "mail_cache_purge_delete_percentage: 100", "mail_driver: maildir",
		"mail_cache_purge_min_size: \"0\"", "mail_cache_purge_min_size: \"32k\"",
		"mail_cache_purge_continued_percentage: 0", "mail_cache_purge_continued_percentage: 200"} {
		if _, err := loadStorage(t, body); err != nil {
			t.Errorf("%q refused: %v", body, err)
		}
	}
}
