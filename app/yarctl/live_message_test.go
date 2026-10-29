package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// seedInbox puts messages in the user's Maildir as an MDA leaves them, then has
// the folder taken up the way a session opens it: an expunge settles it first.
func seedInbox(t *testing.T, b *liveBackend, msgs map[string]string) {
	t.Helper()
	md := filepath.Join(b.root, "example.com", "alice", "Maildir")
	for _, d := range []string{"cur", "new", "tmp"} {
		if err := os.MkdirAll(filepath.Join(md, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, raw := range msgs {
		if err := os.WriteFile(filepath.Join(md, "cur", name), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustYarctl(t, "backend", "folder", "expunge", liveUser, "INBOX")
}

const (
	liveMsg1 = "From: a@example.com\r\nSubject: m1\r\n\r\nbody\r\n"
	liveMsg2 = "From: a@example.com\r\nSubject: m2\r\n\r\nbody\r\n"
)

// Messages, their records and the quota counters read back what the store holds.
func TestMessageVerbsAgainstTheBackend(t *testing.T) {
	b := startLiveBackend(t)
	seedInbox(t, b, map[string]string{
		"1700000000.M1P1.host:2,S": liveMsg1,
		"1700000001.M2P1.host:2,":  liveMsg2,
	})

	var info struct {
		Messages int `json:"messages"`
		Unseen   int `json:"unseen"`
	}
	decode(t, mustYarctl(t, "backend", "folder", "info", liveUser, "INBOX"), &info)
	if info.Messages != 2 || info.Unseen != 1 {
		t.Errorf("INBOX info: %+v, want 2 messages, 1 unseen", info)
	}
	var dump struct {
		Records []struct {
			UID   int      `json:"uid"`
			Flags []string `json:"flags"`
			Size  int      `json:"size"`
		} `json:"records"`
	}
	decode(t, mustYarctl(t, "backend", "index", "dump", liveUser, "INBOX"), &dump)
	if len(dump.Records) != 2 || dump.Records[0].Size != len(liveMsg1) {
		t.Fatalf("index dump: %+v", dump.Records)
	}

	bySubject := map[int]string{}
	for _, r := range dump.Records {
		uid := itoa(r.UID)
		raw := mustYarctl(t, "backend", "mailbox", "message", "get", "raw", liveUser, "INBOX", "--uid", uid)
		bySubject[r.UID] = raw
	}
	if !strings.Contains(bySubject[dump.Records[0].UID]+bySubject[dump.Records[1].UID], "Subject: m1") ||
		!strings.Contains(bySubject[dump.Records[0].UID]+bySubject[dump.Records[1].UID], "Subject: m2") {
		t.Errorf("raw reads: %v", bySubject)
	}
	mime := mustYarctl(t, "backend", "mailbox", "message", "get", "mime", liveUser, "INBOX", "--uid", itoa(dump.Records[0].UID))
	if !strings.Contains(mime, "Subject") {
		t.Errorf("mime form has no headers:\n%s", mime)
	}
	if _, err := yarctl(t, "backend", "mailbox", "message", "get", "raw", liveUser, "INBOX", "--uid", "99"); err == nil {
		t.Error("a read of a uid the folder does not hold succeeded")
	}

	if out := mustYarctl(t, "backend", "quota", "recalc", liveUser); !strings.Contains(out, "2 messages") {
		t.Errorf("quota recalc: %s", out)
	}
	if out := mustYarctl(t, "backend", "quota", "show", liveUser); !strings.Contains(out, "MESSAGE             2") {
		t.Errorf("quota show after recalc:\n%s", out)
	}
}

// An expunge removes exactly the messages marked \Deleted.
func TestExpungeTakesTheDeleted(t *testing.T) {
	b := startLiveBackend(t)
	md := filepath.Join(b.root, "example.com", "alice", "Maildir")
	for _, d := range []string{"cur", "new", "tmp"} {
		if err := os.MkdirAll(filepath.Join(md, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, raw := range map[string]string{
		"1700000000.M1P1.host:2,S": liveMsg1,
		"1700000001.M2P1.host:2,T": liveMsg2,
	} {
		if err := os.WriteFile(filepath.Join(md, "cur", name), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var resp struct {
		Count int `json:"count"`
	}
	decode(t, mustYarctl(t, "backend", "folder", "expunge", liveUser, "INBOX"), &resp)
	if resp.Count != 1 {
		t.Errorf("expunge took %d messages, want the one marked \\Deleted", resp.Count)
	}
	var info struct {
		Messages int `json:"messages"`
	}
	decode(t, mustYarctl(t, "backend", "folder", "info", liveUser, "INBOX"), &info)
	if info.Messages != 1 {
		t.Errorf("INBOX holds %d messages after the expunge, want 1", info.Messages)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
