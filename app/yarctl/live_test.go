package main

import (
	"bytes"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/backendapi"
	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/dict"
	"github.com/yarilomail/yarilo/v2/pkg/dict/memory"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// liveBackend is a real yarilo-backend-api over maildir and the file index in
// a temp root, with yarctl pointed at it; every command crosses HTTP.
type liveBackend struct {
	root   string
	shared dict.Dict
}

func startLiveBackend(t *testing.T) *liveBackend {
	t.Helper()
	root := t.TempDir()
	shared, err := memory.New(dict.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shared.Close() })
	s := backendapi.New(backendapi.Options{
		Token:        "live-token",
		Dicts:        map[string]dict.Dict{"shared": shared},
		Mailbox:      mailbox.Validating(maildir.New(), mailbox.DefaultNameRules()),
		Index:        file.New(),
		Resolver:     &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"},
		Namespaces:   []config.NamespaceConfig{{Type: "personal", Prefix: "", Separator: "/", List: "yes", Inbox: true}},
		MetadataDict: shared,
		SharedDict:   shared,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	oldURL, oldToken, oldRoute, oldFormat := backendAPIURL, backendAPIToken, routeByUser, outputFormat
	backendAPIURL, backendAPIToken, routeByUser, outputFormat = ts.URL, "live-token", false, "human"
	t.Cleanup(func() {
		backendAPIURL, backendAPIToken, routeByUser, outputFormat = oldURL, oldToken, oldRoute, oldFormat
	})
	return &liveBackend{root: root, shared: shared}
}

// yarctl runs one command line through dispatch and returns what it printed.
func yarctl(t *testing.T, args ...string) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	done := make(chan []byte)
	go func() { b, _ := io.ReadAll(r); done <- b }()
	cmdErr := dispatch(args)
	os.Stdout = stdout
	_ = w.Close()
	out := <-done
	return string(bytes.TrimSpace(out)), cmdErr
}

func mustYarctl(t *testing.T, args ...string) string {
	t.Helper()
	out, err := yarctl(t, args...)
	if err != nil {
		t.Fatalf("yarctl %s: %v", strings.Join(args, " "), err)
	}
	return out
}
