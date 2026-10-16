package login

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
)

// disabledPassdb accepts alice's password and marks her nologin, as a passdb
// column or extra field does.
type disabledPassdb struct{ nologin string }

func (d disabledPassdb) Authenticate(req *protocol.Request) (protocol.Result, error) {
	if req.Username != "alice" || req.Password != "secret" {
		return protocol.ResultNext, nil
	}
	req.Fields.Set("user", "alice")
	if d.nologin != "" {
		req.Fields.Set("nologin", d.nologin)
	}
	return protocol.ResultOK, nil
}

// A passdb nologin, sent as nologin=yes, refuses the login with exactly a wrong
// password's answer; lost, the login goes on to the tokenless refusal (#2229).
func TestAPassdbNologinRefusesTheLogin(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go protocol.NewServer([]protocol.Passdb{disabledPassdb{nologin: "yes"}}).Serve(ctx, ln) //nolint:errcheck
	t.Cleanup(func() { cancel(); ln.Close() })                                              //nolint:errcheck

	plain := func(pw string) string { return base64.StdEncoding.EncodeToString([]byte("\x00alice\x00" + pw)) }
	for _, tc := range []struct {
		proto Protocol
		login func(pw string) []string // lines after the greeting; the last answer is judged
	}{
		{ProtocolIMAP, func(pw string) []string { return []string{"a1 LOGIN alice " + pw} }},
		{ProtocolPOP3, func(pw string) []string { return []string{"USER alice", "PASS " + pw} }},
		{ProtocolSubmission, func(pw string) []string { return []string{"EHLO client", "AUTH PLAIN " + plain(pw)} }},
	} {
		t.Run(string(tc.proto), func(t *testing.T) {
			wrong := answer(t, tc.proto, ln.Addr().String(), tc.login("wrong"))
			disabled := answer(t, tc.proto, ln.Addr().String(), tc.login("secret"))
			if disabled != wrong {
				t.Errorf("the disabled account answered %q, a wrong password %q", disabled, wrong)
			}
		})
	}
}

// answer runs one login and returns the proxy's last reply to it.
func answer(t *testing.T, proto Protocol, authAddr string, lines []string) string {
	t.Helper()
	srv := New(testOpts(Options{Protocol: proto, AuthAddr: authAddr, BackendAddr: "127.0.0.1:1", LocalIP: "127.0.0.1"}))
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() }) //nolint:errcheck
	go srv.handleConn(server)
	client.SetDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck
	rd := bufio.NewReader(client)
	rd.ReadString('\n') //nolint:errcheck // greeting
	var last string
	for _, line := range lines {
		fmt.Fprintf(client, "%s\r\n", line)
		last = finalLine(t, rd)
	}
	return last
}

// finalLine skips an SMTP multi-line reply's continuation lines.
func finalLine(t *testing.T, rd *bufio.Reader) string {
	t.Helper()
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if len(line) > 3 && line[3] == '-' && line[0] >= '2' && line[0] <= '5' {
			continue
		}
		return line
	}
}
