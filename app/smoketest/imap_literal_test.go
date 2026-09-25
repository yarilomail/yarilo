package main

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

// A literal belongs to the response it appears in; a row reading line by line
// judges half an answer (#2008).
func TestCmdJoinsLiterals(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() }) //nolint:errcheck

	go func() {
		br := bufio.NewReader(server)
		br.ReadString('\n') //nolint:errcheck
		server.Write([]byte("* 1 FETCH (ENVELOPE (\"date\" {6}\r\nПрив ((NIL)))" +
			" BODYSTRUCTURE (\"text\" \"plain\"))\r\nS0001 OK done\r\n")) //nolint:errcheck
	}()

	c := &imapClient{conn: client, r: bufio.NewReader(client)}
	client.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	lines, err := c.cmd("FETCH 1 (ENVELOPE BODYSTRUCTURE)")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("the answer arrived as %d lines, want one: %q", len(lines), lines)
	}
	for _, item := range []string{"ENVELOPE", "BODYSTRUCTURE", "Прив"} {
		if !strings.Contains(lines[0], item) {
			t.Errorf("the joined line lacks %s: %q", item, lines[0])
		}
	}
}
