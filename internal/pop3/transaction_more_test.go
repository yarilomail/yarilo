package pop3

import (
	"bytes"
	"strings"
	"testing"
)

// TOP sends the header, the blank line and the first n body lines,
// dot-stuffed, whatever line ending the stored message uses.
func TestWriteTopLines(t *testing.T) {
	tests := []struct {
		name, msg string
		n         int
		want      string
	}{
		{"CRLF, one line", "A: 1\r\nB: 2\r\n\r\none\r\ntwo\r\n", 1, "A: 1\r\nB: 2\r\n\r\none\r\n.\r\n"},
		{"no body lines", "A: 1\r\n\r\none\r\n", 0, "A: 1\r\n\r\n.\r\n"},
		{"more than there are", "A: 1\r\n\r\none\r\n", 5, "A: 1\r\n\r\none\r\n.\r\n"},
		{"dot-stuffed", "A: 1\r\n\r\n.hidden\r\nnext\r\n", 2, "A: 1\r\n\r\n..hidden\r\nnext\r\n.\r\n"},
		{"LF only", "A: 1\nB: 2\n\none\ntwo\n", 1, "A: 1\r\nB: 2\r\n\r\none\r\n.\r\n"},
		{"header only", "A: 1\r\n", 3, "A: 1\r\n\r\n.\r\n"},
	}
	for _, tc := range tests {
		var b bytes.Buffer
		writeTopLines(&b, []byte(tc.msg), tc.n)
		if b.String() != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, b.String(), tc.want)
		}
	}
}

// The per-message forms of LIST and UIDL answer for that message alone and
// refuse a number that is not one, deleted or out of range; LAST reports the
// highest message read, and only when it is enabled.
func TestPerMessageCommands(t *testing.T) {
	opts, _ := maildirStandN(t, func(o *Options) { o.EnableLast = true }, 2)
	c, r := newPOP3Session(t, opts)
	login(t, c, r, "u@x", "p")

	send(t, c, "UIDL")
	readline(t, r)
	all := readUntilDot(t, r)
	if len(all) != 2 {
		t.Fatalf("UIDL: %q", all)
	}
	send(t, c, "UIDL 2")
	if got := readline(t, r); got != "+OK "+all[1] {
		t.Errorf("UIDL 2 = %q, want +OK %s", got, all[1])
	}
	send(t, c, "LIST 1")
	if got := readline(t, r); !strings.HasPrefix(got, "+OK 1 ") {
		t.Errorf("LIST 1 = %q", got)
	}
	send(t, c, "LAST")
	if got := readline(t, r); got != "+OK 0" {
		t.Errorf("LAST before any read = %q, want +OK 0", got)
	}
	send(t, c, "RETR 2")
	readline(t, r)
	readUntilDot(t, r)
	send(t, c, "LAST")
	if got := readline(t, r); got != "+OK 2" {
		t.Errorf("LAST after RETR 2 = %q, want +OK 2", got)
	}
	send(t, c, "TOP 1 0")
	if got := readline(t, r); !strings.HasPrefix(got, "+OK") {
		t.Fatalf("TOP 1 0 = %q", got)
	}
	if lines := readUntilDot(t, r); len(lines) < 2 || lines[len(lines)-1] != "" || !strings.HasPrefix(lines[1], "Subject: msg 1") {
		t.Errorf("TOP 1 0 sent %q, want the header and the blank line only", lines)
	}
	send(t, c, "DELE 1")
	readline(t, r)
	for _, cmd := range []string{"LIST 1", "UIDL 1", "LIST 3", "UIDL 0", "LIST x", "TOP 1 0", "TOP 2", "TOP 2 x"} {
		send(t, c, cmd)
		if got := readline(t, r); !strings.HasPrefix(got, "-ERR") {
			t.Errorf("%s = %q, want -ERR", cmd, got)
		}
	}
}

// Without pop3_enable_last the command is unknown.
func TestLastNeedsTheKey(t *testing.T) {
	opts, _ := maildirStandN(t, nil, 1)
	c, r := newPOP3Session(t, opts)
	login(t, c, r, "u@x", "p")
	send(t, c, "LAST")
	if got := readline(t, r); !strings.HasPrefix(got, "-ERR") {
		t.Errorf("LAST without the key = %q, want -ERR", got)
	}
}
