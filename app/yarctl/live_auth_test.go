package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/auth/protocol"
	"github.com/yarilomail/yarilo/v2/internal/auth/scheme"
)

// A verifier the CLI prints is one the auth layer accepts for that password
// and only that one, in the family and with the iterations asked for.
func TestSCRAMVerifierIsAcceptedForItsPasswordOnly(t *testing.T) {
	tests := []struct {
		mech, scheme string
		parse        func(string) bool
	}{
		{"sha256", "SCRAM-SHA-256", func(s string) bool { c, ok := scheme.ParseSCRAMSha256Credentials(s); return ok && c.Iterations == 4096 }},
		{"sha1", "SCRAM-SHA-1", func(s string) bool { c, ok := scheme.ParseSCRAMSha1Credentials(s); return ok && c.Iterations == 4096 }},
	}
	for _, tc := range tests {
		t.Run(tc.mech, func(t *testing.T) {
			out := mustYarctl(t, "auth", "scram-verifier", "--mech", tc.mech, "--iterations", "4096", "--password", "s3cret")
			if !strings.HasPrefix(out, "{"+tc.scheme+"}") || !tc.parse(out) {
				t.Fatalf("verifier %q is not a %s blob of 4096 iterations", out, tc.scheme)
			}
			if !scheme.Verify(out, "s3cret") {
				t.Error("the verifier refuses its own password")
			}
			if scheme.Verify(out, "s3cret ") {
				t.Error("the verifier accepts another password")
			}
		})
	}
	if _, err := yarctl(t, "auth", "scram-verifier", "--mech", "md5", "--password", "x"); err == nil {
		t.Error("an unknown digest family was accepted")
	}
}

// Without --password the verifier reads one line from stdin; an empty line
// is refused rather than turned into a verifier of the empty string.
func TestSCRAMVerifierReadsStdin(t *testing.T) {
	for _, tc := range []struct {
		in      string
		wantErr bool
	}{{"from-stdin\n", false}, {"\n", true}} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.WriteString(tc.in)
		_ = w.Close()
		old := os.Stdin
		os.Stdin = r
		out, err := yarctl(t, "auth", "scram-verifier", "--iterations", "4096")
		os.Stdin = old
		_ = r.Close()
		if tc.wantErr {
			if err == nil {
				t.Errorf("stdin %q: no error", tc.in)
			}
			continue
		}
		if err != nil || !scheme.Verify(out, "from-stdin") {
			t.Errorf("stdin %q: %q, %v; want a verifier of the line read", tc.in, out, err)
		}
	}
}

// cache flush reaches the auth master and reports what it evicted: all of it
// without a mask, only the matching users with one.
func TestAuthCacheFlushAgainstTheMaster(t *testing.T) {
	cache := protocol.NewCache(1<<20, time.Minute, time.Minute)
	for _, u := range []string{"alice@a.test", "alice@b.test", "bob@a.test"} {
		cache.Insert(protocol.MakeCacheKey("imap", u), u, "p", protocol.ResultOK, protocol.NewFields())
	}
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go protocol.NewMasterServer(nil, protocol.WithMasterCache(cache)).ListenAndServe(ctx, addr, nil) //nolint:errcheck
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, err := (&net.Dialer{Timeout: 50 * time.Millisecond}).DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("auth master never listened: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	old := authAddr
	authAddr = addr
	t.Cleanup(func() { authAddr = old })

	if out := mustYarctl(t, "auth", "cache", "flush", "alice@*"); !strings.HasPrefix(out, "2 cache entries flushed matching") {
		t.Errorf("flush alice@*: %q", out)
	}
	if out := mustYarctl(t, "auth", "cache", "flush"); out != "1 cache entries flushed (full)" {
		t.Errorf("full flush: %q, want bob's one entry", out)
	}
	if _, err := yarctl(t, "auth", "cache", "nosuch"); err == nil {
		t.Error("an unknown cache command was accepted")
	}
	if _, err := yarctl(t, "auth", "nosuch"); err == nil {
		t.Error("an unknown auth command was accepted")
	}
}

// The auth TLS flags are all or nothing, and each file is checked for what
// it must hold.
func TestAuthTLSFlags(t *testing.T) {
	dir := t.TempDir()
	notPEM := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(notPEM, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldC, oldK, oldA := authCert, authKey, authCA
	t.Cleanup(func() { authCert, authKey, authCA = oldC, oldK, oldA })
	tests := []struct {
		name          string
		cert, key, ca string
		want          string
	}{
		{"none", "", "", "", ""},
		{"only a cert", "c.pem", "", "", "must all be set together"},
		{"a cert that is not there", filepath.Join(dir, "absent"), filepath.Join(dir, "absent"), notPEM, "load client cert"},
	}
	for _, tc := range tests {
		authCert, authKey, authCA = tc.cert, tc.key, tc.ca
		cfg, err := buildAuthTLS()
		switch {
		case tc.want == "" && (err != nil || cfg != nil):
			t.Errorf("%s: %v, %v; want plain TCP", tc.name, cfg, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}
