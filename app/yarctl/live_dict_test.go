package main

import (
	"context"
	"encoding/base64"
	"os"
	"strings"
	"testing"
)

// withStdin feeds s to a command that reads its script from stdin.
func withStdin(t *testing.T, s string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(s); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old; _ = r.Close() })
}

// The dict verbs change the server's dict and print what it holds.
func TestDictVerbsAgainstTheBackend(t *testing.T) {
	b := startLiveBackend(t)
	ctx := context.Background()
	stored := func(key string) string {
		v, found, err := b.shared.Lookup(ctx, nil, key)
		if err != nil || !found {
			return "(absent)"
		}
		return string(v[0])
	}

	if out := mustYarctl(t, "backend", "dict", "set", "shared", "shared/team/a", "one"); out != "ok" {
		t.Errorf("set printed %q, want ok", out)
	}
	mustYarctl(t, "backend", "dict", "set", "shared", "shared/team/b", "2")
	if got := stored("shared/team/a"); got != "one" {
		t.Errorf("after set the dict holds %q", got)
	}
	if out := mustYarctl(t, "backend", "dict", "lookup", "shared", "shared/team/a"); out != "one" {
		t.Errorf("lookup printed %q, want one", out)
	}
	if out := mustYarctl(t, "backend", "dict", "lookup", "shared", "shared/team/none"); out != "(not found)" {
		t.Errorf("lookup of a missing key printed %q", out)
	}
	if out := mustYarctl(t, "backend", "dict", "iterate", "-recurse", "-sort-key", "shared", "shared/team/"); out != "shared/team/a\tone\nshared/team/b\t2" {
		t.Errorf("iterate printed %q", out)
	}
	if out := mustYarctl(t, "backend", "dict", "atomic-inc", "shared", "shared/team/b", "5"); out != "ok" {
		t.Errorf("atomic-inc printed %q", out)
	}
	if got := stored("shared/team/b"); got != "7" {
		t.Errorf("after atomic-inc the dict holds %q, want 7", got)
	}
	if out := mustYarctl(t, "backend", "dict", "atomic-inc", "shared", "shared/team/none", "1"); !strings.HasPrefix(out, "not-found") {
		t.Errorf("atomic-inc on a missing key printed %q", out)
	}
	if out := mustYarctl(t, "backend", "dict", "unset", "shared", "shared/team/a"); out != "ok" {
		t.Errorf("unset printed %q", out)
	}
	if got := stored("shared/team/a"); got != "(absent)" {
		t.Errorf("after unset the dict holds %q", got)
	}
	if out := mustYarctl(t, "backend", "dict", "exists", "shared"); !strings.Contains(out, "true") {
		t.Errorf("exists for a configured dict printed %q", out)
	}

	withStdin(t, "# two writes and a removal\nset\tshared/batch/x\t"+base64.StdEncoding.EncodeToString([]byte("bx"))+
		"\nset\tshared/batch/y\t"+base64.StdEncoding.EncodeToString([]byte("by"))+"\nunset\tshared/team/b\n")
	if out := mustYarctl(t, "backend", "dict", "commit-batch", "shared"); out != "ok" {
		t.Errorf("commit-batch printed %q", out)
	}
	for key, want := range map[string]string{"shared/batch/x": "bx", "shared/batch/y": "by", "shared/team/b": "(absent)"} {
		if got := stored(key); got != want {
			t.Errorf("after commit-batch %s holds %q, want %q", key, got, want)
		}
	}
}

// A refusal of the server reaches the operator as an error, not as output.
func TestDictErrorsReachTheOperator(t *testing.T) {
	startLiveBackend(t)
	cases := []struct {
		name string
		args []string
	}{
		{"unknown dict", []string{"backend", "dict", "lookup", "nosuch", "k"}},
		{"too few arguments", []string{"backend", "dict", "set", "shared", "k"}},
		{"unknown verb", []string{"backend", "dict", "frobnicate"}},
		{"atomic-inc with a bad delta", []string{"backend", "dict", "atomic-inc", "shared", "k", "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := yarctl(t, tc.args...); err == nil {
				t.Errorf("yarctl %s succeeded", strings.Join(tc.args, " "))
			}
		})
	}
	backendAPIToken = "wrong"
	if _, err := yarctl(t, "backend", "dict", "lookup", "shared", "k"); err == nil {
		t.Error("a wrong token was accepted")
	}
}
