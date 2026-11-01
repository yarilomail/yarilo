package protocol

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/sockaddr"
)

type countingTokenStore struct{ issued atomic.Int32 }

func (c *countingTokenStore) Issue(string, string, string) (string, error) {
	c.issued.Add(1)
	return "tok", nil
}
func (c *countingTokenStore) Validate(string) (string, string, string, bool) {
	return "", "", "", false
}
func (c *countingTokenStore) Consume(string) bool { return false }

// A connection from a uid outside the allowed set gets FAIL on SESSION and no
// token is issued; the allowed uid gets one. Without the gate both get a token.
func TestMasterSessionOnlyForAllowedPeers(t *testing.T) {
	if !sockaddr.PeerSupported {
		t.Skip("no peer credentials on this OS")
	}
	me := uint32(os.Getuid())
	for _, tc := range []struct {
		name   string
		peers  map[uint32]bool
		wantOK bool
		issued int32
	}{
		{"peer allowed", map[uint32]bool{me: true}, true, 1},
		{"peer not allowed", map[uint32]bool{me + 1: true}, false, 0},
		{"nobody allowed", map[uint32]bool{}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := &countingTokenStore{}
			srv := NewMasterServer(nil, WithMasterTokenStore(ts), WithMasterSessionPeers(tc.peers))
			dir, err := os.MkdirTemp("/tmp", "auth")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) })
			path := filepath.Join(dir, "m")
			ln, err := srv.Listen("unix:"+path, sockaddr.Perm{Mode: 0o600}, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			go srv.Serve(ctx, ln) //nolint:errcheck

			conn, err := new(net.Dialer).DialContext(ctx, "unix", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { conn.Close() })
			r := bufio.NewReader(conn)
			readMasterHandshake(t, r)
			send(t, conn, "SESSION\t7\tuser=alice@example.com\tsid=s1\tip=127.0.0.1")
			got := readLine(t, r)
			if ok := strings.HasPrefix(got, "OK\t7\ttoken="); ok != tc.wantOK {
				t.Errorf("SESSION answered %q, want OK=%v", got, tc.wantOK)
			}
			if !tc.wantOK && !strings.HasPrefix(got, "FAIL\t7\t") {
				t.Errorf("SESSION answered %q, want FAIL", got)
			}
			if n := ts.issued.Load(); n != tc.issued {
				t.Errorf("tokens issued %d, want %d", n, tc.issued)
			}
		})
	}
}
