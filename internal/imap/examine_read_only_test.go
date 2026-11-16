package imap_test

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
)

// send runs one tagged command and returns its untagged lines and the word of
// its tagged reply ("OK", "NO", "BAD"); a refusal is a result here, not a fault.
func send(t *testing.T, conn net.Conn, rd *bufio.Reader, tag, cmd string) ([]string, string) {
	t.Helper()
	if _, err := fmt.Fprintf(conn, "%s %s\r\n", tag, cmd); err != nil {
		t.Fatalf("write %s: %v", cmd, err)
	}
	var lines []string
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("read after %s: %v", cmd, err)
		}
		line = strings.TrimRight(line, "\r\n")
		if rest, ok := strings.CutPrefix(line, tag+" "); ok {
			word, _, _ := strings.Cut(rest, " ")
			return lines, word
		}
		lines = append(lines, line)
	}
}

// A folder opened by EXAMINE keeps its state (RFC 9051 §6.3.3): CLOSE expunges
// nothing, STORE, EXPUNGE and MOVE are refused, FETCH BODY[] sets no \Seen, and
// a later SELECT writes again. Both messages are \Deleted, so any expunge shows.
func TestExamineKeepsTheFolderAsItIs(t *testing.T) {
	for _, tc := range []struct {
		name string
		// cmds run under EXAMINE, each with the reply it must get.
		cmds [][2]string
		// exists is how many messages a fresh SELECT then finds.
		exists int
		// flagged and seen: whether message 1 then carries \Flagged / \Seen.
		flagged, seen bool
	}{
		{"CLOSE leaves the messages", [][2]string{{"CLOSE", "OK"}}, 2, false, false},
		{"STORE is refused", [][2]string{{`STORE 1 +FLAGS (\Flagged)`, "NO"}}, 2, false, false},
		{"EXPUNGE is refused", [][2]string{{"EXPUNGE", "NO"}, {"UID EXPUNGE 1:*", "NO"}}, 2, false, false},
		{"MOVE is refused", [][2]string{{"MOVE 1 Archive", "NO"}}, 2, false, false},
		{"FETCH BODY[] sets no Seen", [][2]string{{"FETCH 1 (BODY[])", "OK"}}, 2, false, false},
		{"SELECT after EXAMINE writes again", [][2]string{{"SELECT INBOX", "OK"}, {`STORE 1 +FLAGS (\Flagged)`, "OK"}}, 2, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, ard, login := recentServer(t)
			for i, c := range [][2]string{
				{"SELECT INBOX", "OK"},
				{`STORE 1:* +FLAGS.SILENT (\Deleted)`, "OK"},
				{"CREATE Archive", "OK"},
				{"UNSELECT", "OK"},
			} {
				if _, got := send(t, a, ard, fmt.Sprintf("s%d", i), c[0]); got != c[1] {
					t.Fatalf("setup %s: %s", c[0], got)
				}
			}
			b, brd := login()
			if _, got := send(t, b, brd, "e0", "EXAMINE INBOX"); got != "OK" {
				t.Fatalf("EXAMINE: %s", got)
			}
			for i, c := range tc.cmds {
				if _, got := send(t, b, brd, fmt.Sprintf("e%d", i+1), c[0]); got != c[1] {
					t.Errorf("%s: %s, want %s", c[0], got, c[1])
				}
			}

			lines, got := send(t, a, ard, "c1", "SELECT INBOX")
			if got != "OK" {
				t.Fatalf("SELECT to check: %s", got)
			}
			exists := -1
			for _, l := range lines {
				var n int
				if _, err := fmt.Sscanf(l, "* %d EXISTS", &n); err == nil {
					exists = n
				}
			}
			if exists != tc.exists {
				t.Errorf("INBOX holds %d messages, want %d", exists, tc.exists)
			}
			flags, _ := send(t, a, ard, "c2", "FETCH 1 (FLAGS)")
			f := strings.Join(flags, " ")
			if strings.Contains(f, `\Flagged`) != tc.flagged {
				t.Errorf("message 1 flags %q, want \\Flagged %v", f, tc.flagged)
			}
			if strings.Contains(f, `\Seen`) != tc.seen {
				t.Errorf("message 1 flags %q, want \\Seen %v", f, tc.seen)
			}
		})
	}
}

// A size key keeps its bound when another key follows it: SMALLER 1 matches no
// message whatever comes after it.
func TestSearchSmallerSurvivesALaterKey(t *testing.T) {
	for _, tc := range []struct{ query, want string }{
		{"SEARCH SMALLER 1 SINCE 1-Jan-2000", "* SEARCH"},
		{"SEARCH SINCE 1-Jan-2000 SMALLER 1", "* SEARCH"},
		{"SEARCH LARGER 1 SMALLER 1", "* SEARCH"},
		{"SEARCH SINCE 1-Jan-2000", "* SEARCH 1 2"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			a, ard, _ := recentServer(t)
			if _, got := send(t, a, ard, "a1", "SELECT INBOX"); got != "OK" {
				t.Fatalf("SELECT: %s", got)
			}
			lines, got := send(t, a, ard, "a2", tc.query)
			if got != "OK" {
				t.Fatalf("%s: %s", tc.query, got)
			}
			if hit := strings.TrimSpace(lines[len(lines)-1]); hit != tc.want {
				t.Errorf("%s answered %q, want %q", tc.query, hit, tc.want)
			}
		})
	}
}
