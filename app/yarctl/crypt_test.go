package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

// captureStd returns what fn wrote to stdout and stderr.
func captureStd(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	runErr := fn()
	os.Stdout, os.Stderr = oldOut, oldErr
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String(), runErr
}

func TestKeygenKeepsThePrivateKeyInTheFile(t *testing.T) {
	for _, typ := range []string{"x25519", "hybrid"} {
		t.Run(typ, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "key")
			out, err := captureStd(t, func() error { return cryptKeygen([]string{"--type", typ, "--out", path}) })
			if err != nil {
				t.Fatal(err)
			}
			st, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode().Perm() != 0o600 {
				t.Fatalf("mode %o", st.Mode().Perm())
			}
			data, _ := os.ReadFile(path)
			ids, err := age.ParseIdentities(bytes.NewReader(data))
			if err != nil || len(ids) != 1 {
				t.Fatalf("the file holds no identity: %v", err)
			}
			if strings.Contains(out, "AGE-SECRET-KEY") {
				t.Fatal("the private key reached the output")
			}
			if !strings.HasPrefix(strings.TrimSpace(out), "age1") || strings.Count(strings.TrimSpace(out), "\n") != 0 {
				t.Fatalf("output %q, want the public key alone", out)
			}

			before, _ := os.ReadFile(path)
			if _, err := captureStd(t, func() error { return cryptKeygen([]string{"--type", typ, "--out", path}) }); err == nil {
				t.Fatal("an existing key file was not refused")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("the existing key file was changed")
			}
		})
	}
}

func TestKeygenRefusesAnUnknownType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if _, err := captureStd(t, func() error { return cryptKeygen([]string{"--type", "rsa", "--out", path}) }); err == nil {
		t.Fatal("rsa accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a file was written for a refused type")
	}
}
