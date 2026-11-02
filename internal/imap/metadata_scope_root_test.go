package imap_test

import (
	"reflect"
	"sort"
	"testing"

	imaplib "github.com/emersion/go-imap/v2"
)

// A client reads a whole scope by naming its root with DEPTH, on a folder and
// on the server, and gets that scope's entries alone; the root itself holds no
// value and cannot be set (#2489).
func TestMetadataScopeRoot(t *testing.T) {
	c := startMetadataClient(t, "user@test.com", "testpass")
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	v := func(s string) *[]byte { b := []byte(s); return &b }
	if err := c.SetMetadata("INBOX", map[string]*[]byte{
		"/private/comment": v("a"), "/private/vendor/x/y": v("b"), "/shared/comment": v("c"),
	}).Wait(); err != nil {
		t.Fatalf("SETMETADATA INBOX: %v", err)
	}
	if err := c.SetMetadata("", map[string]*[]byte{"/private/comment": v("server")}).Wait(); err != nil {
		t.Fatalf("SETMETADATA server: %v", err)
	}

	tests := []struct {
		folder, entry string
		depth         imaplib.GetMetadataDepth
		want          []string
	}{
		{"INBOX", "/private", imaplib.GetMetadataDepthInfinity, []string{"/private/comment=a", "/private/vendor/x/y=b"}},
		{"INBOX", "/private", imaplib.GetMetadataDepthOne, []string{"/private/comment=a"}},
		{"INBOX", "/private", imaplib.GetMetadataDepthZero, nil},
		{"INBOX", "/shared", imaplib.GetMetadataDepthInfinity, []string{"/shared/comment=c"}},
		{"INBOX", "/private/", imaplib.GetMetadataDepthInfinity, []string{"/private/comment=a", "/private/vendor/x/y=b"}},
		{"", "/private", imaplib.GetMetadataDepthInfinity, []string{"/private/comment=server"}},
	}
	for _, tc := range tests {
		got, err := c.GetMetadata(tc.folder, []string{tc.entry}, &imaplib.GetMetadataOptions{Depth: tc.depth}).Wait()
		if err != nil {
			t.Errorf("GETMETADATA (DEPTH %v) %q %s: %v", tc.depth, tc.folder, tc.entry, err)
			continue
		}
		var entries []string
		for name, val := range got.Entries {
			entries = append(entries, name+"="+string(*val))
		}
		sort.Strings(entries)
		if !reflect.DeepEqual(entries, tc.want) {
			t.Errorf("GETMETADATA (DEPTH %v) %q %s = %q, want %q", tc.depth, tc.folder, tc.entry, entries, tc.want)
		}
	}

	for _, root := range []string{"/private", "/shared/"} {
		if err := c.SetMetadata("INBOX", map[string]*[]byte{root: v("x")}).Wait(); err == nil {
			t.Errorf("SETMETADATA INBOX %s was accepted", root)
		}
	}
}
