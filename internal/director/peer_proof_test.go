package director

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// dialPeerWith sends a ring handshake whose PEER line is built by peerLine
// from this connection's nonce.
func dialPeerWith(t *testing.T, addr string, left Member, peerLine func(nonce string) string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	rd := bufio.NewReader(conn)
	nonce := ""
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("handshake read: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		if n, ok := strings.CutPrefix(line, "RING-NONCE\t"); ok {
			nonce = n
		}
		if line == "DONE" {
			break
		}
	}
	for _, l := range []string{
		fmt.Sprintf("ME\t%s\t%d", left.IP, left.Port),
		fmt.Sprintf("MEMBERS\t%s\t", left.String()),
		peerLine(nonce),
		"DONE",
	} {
		fmt.Fprintf(conn, "%s\n", l)
	}
	return conn
}

// Only a PEER proving the ring secret for this connection makes a ring peer;
// anything else stays a client and its MEMBERS are never merged.
func TestAPeerMustProveTheRingSecret(t *testing.T) {
	left := Member{IP: "9.0.0.1", Port: 9102}
	for _, tc := range []struct {
		name     string
		line     func(nonce string) string
		admitted bool
	}{
		{"bare PEER", func(string) string { return "PEER\t1" }, false},
		{"garbage proof", func(string) string { return "PEER\t1\tzz" }, false},
		{"another secret", func(n string) string {
			return "PEER\t1\t" + hex.EncodeToString(peerProof([]byte("other"), n, left))
		}, false},
		{"another connection's nonce", func(string) string {
			return "PEER\t1\t" + hex.EncodeToString(peerProof([]byte("shared-secret"), "00", left))
		}, false},
		{"a join proof", func(n string) string {
			return "PEER\t1\t" + hex.EncodeToString(joinHMAC([]byte("shared-secret"), n, left))
		}, false},
		{"the right proof", func(n string) string {
			return "PEER\t1\t" + hex.EncodeToString(peerProof([]byte("shared-secret"), n, left))
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, addr, _ := startKillableRingNode(t, "shared-secret", nil, 2)
			dialPeerWith(t, addr, left, tc.line)
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) && len(srv.membership.Members()) != 2 {
				time.Sleep(20 * time.Millisecond)
			}
			if got := len(srv.membership.Members()) == 2; got != tc.admitted {
				t.Fatalf("admitted = %v (members %v), want %v", got, srv.membership.Members(), tc.admitted)
			}
		})
	}
}
