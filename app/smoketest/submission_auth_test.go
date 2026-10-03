package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
)

// fakeSubmission answers each command with the reply mapped to its verb.
func fakeSubmission(t *testing.T, replies map[string]string) net.Conn {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	go func() {
		rd := bufio.NewReader(server)
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			verb := strings.ToUpper(strings.Fields(line)[0])
			fmt.Fprintf(server, "%s\r\n", replies[verb])
		}
	}()
	return client
}

func TestSmtpSubmitTransaction(t *testing.T) {
	ok := map[string]string{"AUTH": "235 2.7.0 ok", "MAIL": "250 ok", "RCPT": "250 ok", "RSET": "250 ok"}
	if err := smtpSubmitTransaction(fakeSubmission(t, ok), "u1@d00001.test", "pw"); err != nil {
		t.Fatalf("accepted transaction: %v", err)
	}

	// What the login pod answers when it cannot reach the backend (#2133).
	refused := map[string]string{"AUTH": "421 4.3.0 Too many failed authentications"}
	err := smtpSubmitTransaction(fakeSubmission(t, refused), "u1@d00001.test", "pw")
	if err == nil || !strings.Contains(err.Error(), "AUTH") || !strings.Contains(err.Error(), "421") {
		t.Fatalf("refused AUTH: err = %v, want one naming AUTH and 421", err)
	}
}
