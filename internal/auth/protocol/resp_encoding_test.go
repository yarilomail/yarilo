package protocol

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The PLAIN response is base64 on the wire; a raw one is refused, so a TAB
// or LF in it can never have framed anything.
func TestThePlainResponseIsBase64(t *testing.T) {
	srv := NewServer([]Passdb{&credPassdb{"alice", "alicepass"}})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)
	conn, sc := dialAndHandshake(t, addr)
	t.Cleanup(func() { conn.Close() }) //nolint:errcheck

	for _, tc := range []struct{ id, resp, want string }{
		{"1", base64.StdEncoding.EncodeToString([]byte("\x00alice\x00alicepass")), "OK\t1"},
		{"2", "\x00alice\x00alicepass", "FAIL\t2"},
	} {
		fmt.Fprintf(conn, "AUTH\t%s\tPLAIN\tservice=imap\tresp=%s\n", tc.id, tc.resp)
		if !sc.Scan() {
			t.Fatalf("no reply: %v", sc.Err())
		}
		if got := sc.Text(); !strings.HasPrefix(got, tc.want) {
			t.Errorf("AUTH %s = %q, want %q", tc.id, got, tc.want)
		}
	}
}
