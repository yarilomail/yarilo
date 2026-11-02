package main

import (
	"strings"
	"testing"
)

// The index repairs run against a real store and report what they found:
// a healthy folder checks clean, a rebuild keeps the UIDs it can, and every
// verb answers its usage on missing arguments rather than calling the API.
func TestIndexVerbsAgainstTheBackend(t *testing.T) {
	b := startLiveBackend(t)
	seedInbox(t, b, map[string]string{
		"1700000000.M1P1.host:2,S": liveMsg1,
		"1700000001.M2P1.host:2,":  liveMsg2,
	})

	var check struct {
		Checked int `json:"checked"`
		Shifted int `json:"shifted"`
	}
	decode(t, mustYarctl(t, "backend", "index", "check", liveUser), &check)
	if check.Checked != 2 || check.Shifted != 0 {
		t.Errorf("index check: %+v, want 2 checked, 0 shifted", check)
	}

	var rebuilt struct {
		Scanned       int `json:"scanned"`
		UIDsPreserved int `json:"uids_preserved"`
		UIDsAssigned  int `json:"uids_assigned"`
	}
	decode(t, mustYarctl(t, "backend", "index", "rebuild", liveUser, "INBOX"), &rebuilt)
	if rebuilt.Scanned != 2 || rebuilt.UIDsPreserved != 2 || rebuilt.UIDsAssigned != 0 {
		t.Errorf("index rebuild: %+v, want 2 scanned, both UIDs kept", rebuilt)
	}
	var dump struct {
		Records []struct {
			UID int `json:"uid"`
		} `json:"records"`
	}
	decode(t, mustYarctl(t, "backend", "index", "dump", liveUser, "INBOX", "--limit", "1"), &dump)
	if len(dump.Records) != 1 || dump.Records[0].UID != 1 {
		t.Errorf("index dump --limit 1: %+v, want UID 1 alone", dump.Records)
	}

	var purge struct {
		Folder  string `json:"folder"`
		Carried int    `json:"carried"`
	}
	decode(t, mustYarctl(t, "backend", "index", "cache-purge", liveUser, "INBOX"), &purge)
	if purge.Folder != "INBOX" {
		t.Errorf("cache-purge: %+v, want INBOX", purge)
	}
	for _, args := range [][]string{
		{"backend", "index", "optimize", liveUser, "INBOX"},
		{"backend", "index", "optimize", liveUser, "--all"},
		{"backend", "index", "rebuild-guid-store", liveUser},
	} {
		mustYarctl(t, args...)
	}
	if _, err := yarctl(t, "backend", "index", "rebuild-storage", liveUser); err == nil || !strings.Contains(err.Error(), "mdbox") {
		t.Errorf("rebuild-storage on maildir = %v, want the refusal naming mdbox", err)
	}
	decode(t, mustYarctl(t, "backend", "index", "dump", liveUser, "INBOX"), &dump)
	if len(dump.Records) != 2 {
		t.Errorf("after the repairs: %d records, want 2", len(dump.Records))
	}

	for _, args := range [][]string{
		{"backend", "index", "dump", liveUser},
		{"backend", "index", "check"},
		{"backend", "index", "rebuild", liveUser},
		{"backend", "index", "rebuild-storage"},
		{"backend", "index", "rebuild-guid-store"},
		{"backend", "index", "optimize", liveUser},
		{"backend", "index", "cache-purge", liveUser},
	} {
		if _, err := yarctl(t, args...); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("yarctl %s = %v, want its usage", strings.Join(args, " "), err)
		}
	}
	if _, err := yarctl(t, "backend", "index", "nosuch"); err == nil {
		t.Error("an unknown index command was accepted")
	}
}
