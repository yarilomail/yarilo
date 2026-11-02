package imap_test

import (
	"testing"

	imaplib "github.com/emersion/go-imap/v2"
)

// A server annotation is the server's: INBOX does not list it at any depth or
// by its stored name, and a client cannot write that name on a folder; the
// server scope itself still reads it back (#2488).
func TestServerMetadataIsNotINBOXs(t *testing.T) {
	c := startMetadataClient(t, "user@test.com", "testpass")
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	sv, own := []byte("server-level"), []byte("inbox's own")
	if err := c.SetMetadata("", map[string]*[]byte{"/private/comment": &sv}).Wait(); err != nil {
		t.Fatalf("SETMETADATA server: %v", err)
	}
	if err := c.SetMetadata("INBOX", map[string]*[]byte{"/private/vendor/other": &own}).Wait(); err != nil {
		t.Fatalf("SETMETADATA INBOX: %v", err)
	}

	tests := []struct {
		entry string
		depth imaplib.GetMetadataDepth
	}{
		{"/private/vendor", imaplib.GetMetadataDepthInfinity},
		{"/private/vendor/yarilo", imaplib.GetMetadataDepthInfinity},
		{"/private/vendor/yarilo/pvt/server", imaplib.GetMetadataDepthOne},
		{"/private/vendor/yarilo/pvt/server/comment", imaplib.GetMetadataDepthZero},
	}
	for _, tc := range tests {
		got, err := c.GetMetadata("INBOX", []string{tc.entry}, &imaplib.GetMetadataOptions{Depth: tc.depth}).Wait()
		if err != nil {
			t.Fatalf("GETMETADATA INBOX %s: %v", tc.entry, err)
		}
		for name := range got.Entries {
			if name == "/private/vendor/yarilo/pvt/server/comment" {
				t.Errorf("GETMETADATA INBOX %s (depth %v) shows the server's annotation", tc.entry, tc.depth)
			}
		}
	}
	got, err := c.GetMetadata("INBOX", []string{"/private/vendor"}, &imaplib.GetMetadataOptions{Depth: imaplib.GetMetadataDepthInfinity}).Wait()
	if err != nil || got.Entries["/private/vendor/other"] == nil {
		t.Errorf("INBOX's own entry under vendor is gone: %#v, %v", got, err)
	}

	x := []byte("forged")
	if err := c.SetMetadata("INBOX", map[string]*[]byte{"/private/vendor/yarilo/pvt/server/comment": &x}).Wait(); err == nil {
		t.Error("a client wrote into the server's subtree through INBOX")
	}
	srv, err := c.GetMetadata("", []string{"/private/comment"}, nil).Wait()
	if err != nil || srv.Entries["/private/comment"] == nil || string(*srv.Entries["/private/comment"]) != "server-level" {
		t.Errorf("the server's annotation reads %#v, %v; want it intact", srv, err)
	}
}
