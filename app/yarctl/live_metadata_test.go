package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An annotation written through the CLI reads back where it was written and
// nowhere else: a folder's entry is not the server's, a private entry is not a
// shared one, and a deleted entry is gone (RFC 5464).
func TestMetadataVerbsAgainstTheBackend(t *testing.T) {
	b := startLiveBackend(t)
	seedInbox(t, b, map[string]string{"1700000000.M1P1.host:2,S": liveMsg1})

	get := func(args ...string) (found bool, value string) {
		t.Helper()
		var r struct {
			Found bool   `json:"found"`
			Value string `json:"value"`
		}
		decode(t, mustYarctl(t, append([]string{"backend", "metadata", "get", liveUser}, args...)...), &r)
		raw, err := base64.StdEncoding.DecodeString(r.Value)
		if err != nil {
			t.Fatalf("value %q: %v", r.Value, err)
		}
		return r.Found, string(raw)
	}

	mustYarctl(t, "backend", "metadata", "set", liveUser, "INBOX", "--entry", "/private/comment", "--value", "on inbox")
	valueFile := filepath.Join(t.TempDir(), "v")
	if err := os.WriteFile(valueFile, []byte("on the server"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustYarctl(t, "backend", "metadata", "set", liveUser, "--entry", "/private/comment", "--value-file", valueFile)
	mustYarctl(t, "backend", "metadata", "set", liveUser, "INBOX", "--entry", "/shared/comment", "--value", "shared one")

	tests := []struct {
		name  string
		args  []string
		found bool
		value string
	}{
		{"folder private", []string{"INBOX", "--entry", "/private/comment"}, true, "on inbox"},
		{"server private", []string{"--entry", "/private/comment"}, true, "on the server"},
		{"folder shared", []string{"INBOX", "--entry", "/shared/comment"}, true, "shared one"},
		{"never written", []string{"INBOX", "--entry", "/private/other"}, false, ""},
	}
	for _, tc := range tests {
		if found, value := get(tc.args...); found != tc.found || value != tc.value {
			t.Errorf("%s: found %v %q, want %v %q", tc.name, found, value, tc.found, tc.value)
		}
	}

	// INBOX lists its own entries and never the server's, which live in its
	// space under a vendor prefix (#2488); the server scope lists them.
	for _, tc := range []struct {
		folder, scope, want string
	}{
		{"INBOX", "private", "/private/comment"},
		{"INBOX", "shared", "/shared/comment"},
		{"", "private", "/private/comment"},
	} {
		var list struct {
			Entries map[string]string `json:"entries"`
		}
		args := []string{"backend", "metadata", "list", liveUser}
		if tc.folder != "" {
			args = append(args, tc.folder)
		}
		decode(t, mustYarctl(t, append(args, "--scope", tc.scope)...), &list)
		if _, ok := list.Entries[tc.want]; !ok || len(list.Entries) != 1 {
			t.Errorf("%s list on %q: %v, want %s alone", tc.scope, tc.folder, list.Entries, tc.want)
		}
	}
	if _, err := yarctl(t, "backend", "metadata", "set", liveUser, "INBOX", "--entry", "/private/vendor/yarilo/pvt/server/comment", "--value", "x"); err == nil {
		t.Error("the server's subtree was written through INBOX")
	}
	if _, err := yarctl(t, "backend", "metadata", "get", liveUser, "INBOX", "--entry", "/private/vendor/yarilo/pvt/server/comment"); err == nil {
		t.Error("INBOX reads the server's annotation by its stored name")
	}

	mustYarctl(t, "backend", "metadata", "del", liveUser, "INBOX", "--entry", "/private/comment")
	if found, _ := get("INBOX", "--entry", "/private/comment"); found {
		t.Error("the deleted folder entry is still there")
	}
	if found, value := get("--entry", "/private/comment"); !found || value != "on the server" {
		t.Errorf("deleting the folder's entry took the server's: %v %q", found, value)
	}

	if _, err := yarctl(t, "backend", "metadata", "get", liveUser, "INBOX", "--entry", "/nowhere/x"); err == nil {
		t.Error("an entry outside /private/ and /shared/ was accepted")
	}
	for _, args := range [][]string{
		{"backend", "metadata", "list"},
		{"backend", "metadata", "get", liveUser},
		{"backend", "metadata", "set", liveUser, "--value", "x"},
		{"backend", "metadata", "delete", liveUser},
	} {
		if _, err := yarctl(t, args...); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("yarctl %s = %v, want its usage", strings.Join(args, " "), err)
		}
	}
	if _, err := yarctl(t, "backend", "metadata", "set", liveUser, "--entry", "/private/x"); err == nil ||
		!strings.Contains(err.Error(), "--value") {
		t.Errorf("set without a value = %v, want it named", err)
	}
	if _, err := yarctl(t, "backend", "metadata", "nosuch"); err == nil {
		t.Error("an unknown metadata command was accepted")
	}
}
