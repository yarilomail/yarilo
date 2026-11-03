package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// yarctlBoth runs one command line and returns what it printed on stdout and
// stderr together: usage goes to either, depending on the plane.
func yarctlBoth(t *testing.T, args ...string) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	done := make(chan []byte)
	go func() { b, _ := io.ReadAll(r); done <- b }()
	cmdErr := dispatch(args)
	os.Stdout, os.Stderr = stdout, stderr
	_ = w.Close()
	return string(bytes.TrimSpace(<-done)), cmdErr
}

// A command given no subcommand answers with its help and succeeds, and the
// help names the command it belongs to, so an operator lands somewhere
// useful rather than on an error.
func TestEveryCommandAnswersWithItsHelp(t *testing.T) {
	tests := []struct {
		args []string
		says string
	}{
		{[]string{"backend"}, "backend"},
		{[]string{"backend", "dict"}, "dict"},
		{[]string{"backend", "folder"}, "folder"},
		{[]string{"backend", "user"}, "user"},
		{[]string{"backend", "index"}, "index"},
		{[]string{"backend", "mdbox"}, "mdbox"},
		{[]string{"backend", "subs"}, "subscriptions"},
		{[]string{"backend", "specialuse"}, "specialuse"},
		{[]string{"backend", "metadata"}, "metadata"},
		{[]string{"backend", "acl"}, "acl"},
		{[]string{"backend", "sessions"}, "sessions"},
		{[]string{"backend", "quota"}, "quota"},
		{[]string{"backend", "fts"}, "fts"},
		{[]string{"director"}, "director"},
		{[]string{"auth"}, "auth"},
		{[]string{"auth", "cache"}, "cache"},
		{[]string{"backend", "mailbox"}, "mailbox"},
		{[]string{"backend", "quota", "clone"}, "clone"},
		{[]string{"director", "backends"}, "backends"},
		{[]string{"director", "users"}, "users"},
		{[]string{"director", "domains"}, "domains"},
		{[]string{"director", "ring"}, "ring"},
		{[]string{"warden"}, "warden"},
	}
	for _, tc := range tests {
		out, err := yarctlBoth(t, tc.args...)
		if err != nil {
			t.Errorf("yarctl %s: %v", strings.Join(tc.args, " "), err)
			continue
		}
		if !strings.Contains(out, "yarctl") || !strings.Contains(out, tc.says) {
			t.Errorf("yarctl %s printed no help naming %q:\n%s", strings.Join(tc.args, " "), tc.says, out)
		}
	}
	if _, err := yarctlBoth(t, "nosuch"); err == nil || !strings.Contains(err.Error(), "unknown plane") {
		t.Errorf("an unknown plane = %v", err)
	}
	// A word a command does not know is an error, with what it does know.
	for _, args := range [][]string{
		{"backend", "nosuch"}, {"backend", "mailbox", "nosuch"}, {"backend", "quota", "clone", "nosuch"},
		{"director", "backends", "nosuch"}, {"director", "users", "nosuch"}, {"director", "domains", "nosuch"},
		{"director", "ring", "nosuch"}, {"warden", "nosuch"},
	} {
		if _, err := yarctlBoth(t, args...); err == nil || !strings.Contains(err.Error(), "available") {
			t.Errorf("yarctl %s = %v, want an error naming what is available", strings.Join(args, " "), err)
		}
	}
}

// With the plane fixed by YARILO_ADMIN_TYPE the plane word is optional, a
// word naming another plane is refused, and an unknown type is named.
func TestTheImplicitPlane(t *testing.T) {
	t.Setenv("YARILO_ADMIN_TYPE", "backend")
	for _, args := range [][]string{{"quota"}, {"backend", "quota"}} {
		if out, err := yarctlBoth(t, args...); err != nil || !strings.Contains(out, "quota") {
			t.Errorf("yarctl %s under the backend plane = %q, %v", strings.Join(args, " "), out, err)
		}
	}
	if _, err := yarctlBoth(t, "director", "status"); err == nil || !strings.Contains(err.Error(), "different one") {
		t.Errorf("another plane's word = %v", err)
	}
	t.Setenv("YARILO_ADMIN_TYPE", "mars")
	if _, err := yarctlBoth(t, "status"); err == nil || !strings.Contains(err.Error(), "mars") {
		t.Errorf("an unknown plane type = %v", err)
	}
}

func TestParseUIDList(t *testing.T) {
	tests := []struct {
		in      string
		want    []uint32
		wantErr bool
	}{
		{"1,2, 3", []uint32{1, 2, 3}, false},
		{"7", []uint32{7}, false},
		{"", nil, false},
		{"1,,2", []uint32{1, 2}, false},
		{"1,x", nil, true},
		{"4294967296", nil, true},
	}
	for _, tc := range tests {
		got, err := parseUIDList(tc.in)
		if (err != nil) != tc.wantErr || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseUIDList(%q) = %v, %v; want %v (error %v)", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}

// folder stats and an expunge of named UIDs run against the store.
func TestFolderStatsAndExpungeByUID(t *testing.T) {
	b := startLiveBackend(t)
	seedInbox(t, b, map[string]string{
		"1700000000.M1P1.host:2,S": liveMsg1,
		"1700000001.M2P1.host:2,":  liveMsg2,
	})
	var stats struct {
		Messages int `json:"messages"`
	}
	decode(t, mustYarctl(t, "backend", "folder", "stats", liveUser, "INBOX"), &stats)
	if stats.Messages != 2 {
		t.Errorf("folder stats: %+v, want 2 messages", stats)
	}
	// The UID set narrows the expunge to the \Deleted messages in it.
	cur := filepath.Join(b.root, "example.com", "alice", "Maildir", "cur")
	if err := os.Rename(filepath.Join(cur, "1700000000.M1P1.host:2,S"), filepath.Join(cur, "1700000000.M1P1.host:2,ST")); err != nil {
		t.Fatal(err)
	}
	var info struct {
		Messages int `json:"messages"`
	}
	for _, step := range []struct {
		uids string
		want int
	}{{"2", 2}, {"1", 1}} {
		mustYarctl(t, "backend", "folder", "expunge", liveUser, "INBOX", "--uids", step.uids)
		decode(t, mustYarctl(t, "backend", "folder", "info", liveUser, "INBOX"), &info)
		if info.Messages != step.want {
			t.Errorf("after expunge --uids %s: %d messages, want %d", step.uids, info.Messages, step.want)
		}
	}
	if _, err := yarctl(t, "backend", "folder", "expunge", liveUser, "INBOX", "--uids", "1,x"); err == nil {
		t.Error("a malformed UID list was sent")
	}
}
