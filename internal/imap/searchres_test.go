package imap_test

import (
	"regexp"
	"strings"
	"testing"
)

// searchResSession leaves INBOX with UIDs 2..5 at sequence numbers 1..4 and
// $ saved as UIDs 3,5, so a $ read as sequence numbers picks other messages.
func searchResSession(t *testing.T) *rawConn {
	t.Helper()
	rc := dialRaw(t, startNotifyServer(t))
	rc.login()
	for range 5 {
		rc.appendMsg("INBOX")
	}
	rc.cmd("CREATE Other")
	rc.cmd("SELECT INBOX")
	rc.cmd(`UID STORE 1 +FLAGS (\Deleted)`)
	rc.cmd("EXPUNGE")
	rc.cmd("UID SEARCH RETURN (SAVE) UID 3,5")
	return rc
}

var esearchAll = regexp.MustCompile(`(?m)^\* ESEARCH .*?(?: ALL (\S+))?$`)

// searchAll is the ALL set of the ESEARCH line in out, "" when it names none.
func searchAll(t *testing.T, out string) string {
	t.Helper()
	m := esearchAll.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no ESEARCH line in:\n%s", out)
	}
	return m[1]
}

var fetchSeqUID = regexp.MustCompile(`(?m)^\* (\d+) FETCH \(.*UID (\d+)`)

// fetched lists "seq:uid" for every FETCH line in out.
func fetched(out string) string {
	var got []string
	for _, m := range fetchSeqUID.FindAllStringSubmatch(out, -1) {
		got = append(got, m[1]+":"+m[2])
	}
	return strings.Join(got, " ")
}

// Every command taking a message set acts on the saved UIDs for $ (RFC 5182 §2.1).
func TestSearchResDollarInCommands(t *testing.T) {
	cases := []struct {
		name  string
		check func(t *testing.T, rc *rawConn) (got, want string)
	}{
		{"UID FETCH $", func(t *testing.T, rc *rawConn) (string, string) {
			return fetched(rc.cmd("UID FETCH $ (FLAGS)")), "2:3 4:5"
		}},
		{"FETCH $ addresses by UID", func(t *testing.T, rc *rawConn) (string, string) {
			return fetched(rc.cmd("FETCH $ (UID)")), "2:3 4:5"
		}},
		{"UID STORE $", func(t *testing.T, rc *rawConn) (string, string) {
			rc.cmd(`UID STORE $ +FLAGS.SILENT (\Flagged)`)
			return searchAll(t, rc.cmd("UID SEARCH RETURN (ALL) FLAGGED")), "3,5"
		}},
		{"UID COPY $", func(t *testing.T, rc *rawConn) (string, string) {
			out := rc.cmd("UID COPY $ Other")
			m := regexp.MustCompile(`COPYUID \d+ (\S+) `).FindStringSubmatch(out)
			if m == nil {
				t.Fatalf("no COPYUID in:\n%s", out)
			}
			return m[1], "3,5"
		}},
		{"UID MOVE $", func(t *testing.T, rc *rawConn) (string, string) {
			rc.cmd("UID MOVE $ Other")
			return searchAll(t, rc.cmd("UID SEARCH RETURN (ALL) ALL")), "2,4"
		}},
		{"UID EXPUNGE $", func(t *testing.T, rc *rawConn) (string, string) {
			rc.cmd(`UID STORE 1:* +FLAGS.SILENT (\Deleted)`)
			rc.cmd("UID EXPUNGE $")
			return searchAll(t, rc.cmd("UID SEARCH RETURN (ALL) ALL")), "2,4"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, want := tc.check(t, searchResSession(t)); got != want {
				t.Errorf("acted on %q, want %q", got, want)
			}
		})
	}
}

// SAVE alone answers nothing and keeps the set, reading the old $ first;
// SELECT and a search with no hits leave $ empty (RFC 5182 §2.1).
func TestSearchResSaved(t *testing.T) {
	cases := []struct {
		name, cmd string
		silent    bool
		want      string
	}{
		{"SAVE alone answers no ESEARCH", "UID SEARCH RETURN (SAVE) UID 2", true, "2"},
		{"SAVE reads the old $", "UID SEARCH RETURN (SAVE) $ UID 5", true, "5"},
		{"SELECT empties $", "SELECT INBOX", false, ""},
		{"no hits empties $", "UID SEARCH RETURN (SAVE) UID 99", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc := searchResSession(t)
			out := rc.cmd(tc.cmd)
			if tc.silent && strings.Contains(out, "ESEARCH") {
				t.Errorf("%s answered:\n%s", tc.cmd, out)
			}
			if got := searchAll(t, rc.cmd("UID SEARCH RETURN (ALL) $")); got != tc.want {
				t.Errorf("$ holds %q after %s, want %q", got, tc.cmd, tc.want)
			}
		})
	}
}
