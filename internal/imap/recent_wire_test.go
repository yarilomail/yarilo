package imap_test

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// recentServer is two messages in INBOX, a virtual folder over it and the real
// lock service; each login is a session of its own.
func recentServer(t *testing.T) (net.Conn, *bufio.Reader, func() (net.Conn, *bufio.Reader)) {
	t.Helper()
	return recentServerWith(t, "INBOX\n")
}

// recentServerWith is recentServer with Virtual/All configured as config.
func recentServerWith(t *testing.T, config string) (net.Conn, *bufio.Reader, func() (net.Conn, *bufio.Reader)) {
	t.Helper()
	conn, rd := virtualServerWith(t, map[string]string{"All": config},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "one", nil)
			saveInto(t, box, ui, "INBOX", 2, "two", nil)
		}, nil, virtualLocks(t))
	addr := lastVirtualAddr
	return conn, rd, func() (net.Conn, *bufio.Reader) { return loginTo(t, addr) }
}

// recentOf is the n of "* n RECENT", or -1 when the reply carries none.
func recentOf(lines []string) int {
	for _, l := range lines {
		var n int
		if _, err := fmt.Sscanf(l, "* %d RECENT", &n); err == nil {
			return n
		}
	}
	return -1
}

// flaggedRecent is the sequence numbers a FETCH answered with \Recent.
func flaggedRecent(lines []string) string {
	var seqs []string
	for _, l := range lines {
		if strings.Contains(l, " FETCH ") && strings.Contains(l, `\Recent`) {
			seqs = append(seqs, strings.Fields(l)[1])
		}
	}
	return strings.Join(seqs, ",")
}

func searchHits(lines []string) string {
	for _, l := range lines {
		if strings.HasPrefix(l, "* SEARCH") {
			return strings.TrimSpace(strings.TrimPrefix(l, "* SEARCH"))
		}
	}
	return "?"
}

// The first SELECT after delivery takes the messages as \Recent; a later one
// sees none, while the first keeps them (RFC 3501 §2.3.2).
func TestTheFirstSelectTakesRecent(t *testing.T) {
	a, ard, login := recentServer(t)
	if got := recentOf(command(t, a, ard, "a2", "SELECT INBOX")); got != 2 {
		t.Errorf("first SELECT: RECENT %d, want 2", got)
	}
	b, brd := login()
	if got := recentOf(command(t, b, brd, "b2", "SELECT INBOX")); got != 0 {
		t.Errorf("second SELECT: RECENT %d, want 0", got)
	}
	if got := flaggedRecent(command(t, a, ard, "a3", "FETCH 1:* (FLAGS)")); got != "1,2" {
		t.Errorf("first session FETCH: \\Recent on %q, want 1,2", got)
	}
	if got := flaggedRecent(command(t, b, brd, "b3", "FETCH 1:* (FLAGS)")); got != "" {
		t.Errorf("second session FETCH: \\Recent on %q, want none", got)
	}
}

// EXAMINE and STATUS read \Recent and leave it for the next SELECT.
func TestExamineAndStatusLeaveRecent(t *testing.T) {
	a, ard, login := recentServer(t)
	for _, l := range command(t, a, ard, "a2", "STATUS INBOX (RECENT)") {
		if strings.HasPrefix(l, "* STATUS") && !strings.Contains(l, "RECENT 2") {
			t.Errorf("STATUS answered %q, want RECENT 2", l)
		}
	}
	if got := recentOf(command(t, a, ard, "a3", "EXAMINE INBOX")); got != 2 {
		t.Errorf("EXAMINE: RECENT %d, want 2", got)
	}
	if got := flaggedRecent(command(t, a, ard, "a4", "FETCH 1:* (FLAGS)")); got != "1,2" {
		t.Errorf("EXAMINE FETCH: \\Recent on %q, want 1,2", got)
	}
	b, brd := login()
	if got := recentOf(command(t, b, brd, "b2", "SELECT INBOX")); got != 2 {
		t.Errorf("SELECT after EXAMINE and STATUS: RECENT %d, want 2", got)
	}
}

// Mail delivered while a SELECT holds the folder is recent to that session,
// and to no session selecting after it.
func TestArrivalsAreRecentToTheSelectingSession(t *testing.T) {
	a, ard, login := recentServer(t)
	command(t, a, ard, "a2", "SELECT INBOX")
	b, brd := login()
	appendInbox(t, b, brd, "b2", "three")
	lines := command(t, a, ard, "a3", "NOOP")
	if got := linesWith(lines, " EXISTS"); len(got) != 1 || got[0] != "* 3 EXISTS" {
		t.Errorf("NOOP answered %v, want * 3 EXISTS", got)
	}
	if got := recentOf(lines); got != 3 {
		t.Errorf("NOOP: RECENT %d, want 3", got)
	}
	if got := flaggedRecent(command(t, a, ard, "a4", "FETCH 3 (FLAGS)")); got != "3" {
		t.Errorf("FETCH 3: \\Recent on %q, want 3", got)
	}
	c, crd := login()
	if got := recentOf(command(t, c, crd, "c2", "SELECT INBOX")); got != 0 {
		t.Errorf("SELECT after the arrival was taken: RECENT %d, want 0", got)
	}
}

// Of sessions selecting at once each message goes to exactly one.
func TestConcurrentSelectsShareNoRecent(t *testing.T) {
	const sessions = 4
	for round := 0; round < 5; round++ {
		_, _, login := recentServer(t)
		conns := make([]net.Conn, sessions)
		rds := make([]*bufio.Reader, sessions)
		for i := range conns {
			conns[i], rds[i] = login()
		}
		got := make([]int, sessions)
		errs := make([]error, sessions)
		var wg sync.WaitGroup
		for i := range conns {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				got[i], errs[i] = rawSelectRecent(conns[i], rds[i], fmt.Sprintf("s%d", i))
			}(i)
		}
		wg.Wait()
		total, holders := 0, 0
		for i, n := range got {
			if errs[i] != nil {
				t.Fatalf("round %d session %d: %v", round, i, errs[i])
			}
			total += n
			if n > 0 {
				holders++
			}
		}
		if total != 2 || holders != 1 {
			t.Fatalf("round %d: RECENT per session %v, want 2 in one session and 0 elsewhere", round, got)
		}
	}
}

// rawSelectRecent is SELECT INBOX without t: it runs in a goroutine.
func rawSelectRecent(conn net.Conn, rd *bufio.Reader, tag string) (int, error) {
	if _, err := fmt.Fprintf(conn, "%s SELECT INBOX\r\n", tag); err != nil {
		return 0, err
	}
	var lines []string
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return 0, err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, tag+" ") {
			if !strings.HasPrefix(line, tag+" OK") {
				return 0, fmt.Errorf("SELECT answered %q", line)
			}
			return recentOf(lines), nil
		}
		lines = append(lines, line)
	}
}

// IMAP4rev2 has no \Recent: no RECENT response and no flag, though the
// selection still takes the messages from the next session.
func TestARev2SessionSeesNoRecent(t *testing.T) {
	a, ard, login := recentServer(t)
	command(t, a, ard, "a2", "ENABLE IMAP4rev2")
	if got := recentOf(command(t, a, ard, "a3", "SELECT INBOX")); got != -1 {
		t.Errorf("rev2 SELECT: RECENT %d, want no RECENT response", got)
	}
	if got := flaggedRecent(command(t, a, ard, "a4", "FETCH 1:* (FLAGS)")); got != "" {
		t.Errorf("rev2 FETCH: \\Recent on %q, want none", got)
	}
	b, brd := login()
	if got := recentOf(command(t, b, brd, "b2", "SELECT INBOX")); got != 0 {
		t.Errorf("SELECT after a rev2 SELECT: RECENT %d, want 0", got)
	}
}

// SEARCH RECENT, NEW and OLD read the session's \Recent: a message delivered
// while the first session holds the folder goes to whoever selects it first.
func TestSearchReadsRecent(t *testing.T) {
	a, ard, login := recentServer(t)
	command(t, a, ard, "a2", "SELECT INBOX")
	command(t, a, ard, "a3", `STORE 1 +FLAGS.SILENT (\Seen)`)
	b, brd := login()
	appendInbox(t, b, brd, "b2", "three")
	command(t, b, brd, "b3", "SELECT INBOX")
	cases := []struct {
		session  string
		criteria string
		want     string
	}{
		{"first", "RECENT", "1 2"},
		{"first", "NEW", "2"},
		{"first", "OLD", "3"},
		{"second", "RECENT", "3"},
		{"second", "NEW", "3"},
		{"second", "OLD", "1 2"},
	}
	for i, tc := range cases {
		conn, rd := a, ard
		if tc.session == "second" {
			conn, rd = b, brd
		}
		if got := searchHits(command(t, conn, rd, fmt.Sprintf("q%d", i), "SEARCH "+tc.criteria)); got != tc.want {
			t.Errorf("%s SEARCH %s = %q, want %q", tc.session, tc.criteria, got, tc.want)
		}
	}
}

// A client cannot set \Recent (RFC 3501 §2.3.2), in STORE or APPEND.
func TestAClientCannotSetRecent(t *testing.T) {
	a, ard, _ := recentServer(t)
	command(t, a, ard, "a2", "SELECT INBOX")
	fmt.Fprintf(a, "a3 STORE 1 +FLAGS (\\Recent)\r\n")
	if line := readTagged(t, ard, "a3"); !strings.HasPrefix(line, "a3 BAD") {
		t.Errorf("STORE +FLAGS (\\Recent) answered %q, want BAD", line)
	}
	raw := "Subject: r\r\n\r\nb\r\n"
	fmt.Fprintf(a, "a4 APPEND INBOX (\\Recent) {%d+}\r\n%s\r\n", len(raw), raw)
	if line := readTagged(t, ard, "a4"); !strings.HasPrefix(line, "a4 BAD") {
		t.Errorf("APPEND (\\Recent) answered %q, want BAD", line)
	}
}

// A virtual mailbox keeps \Recent of its own: the first SELECT takes its
// records, a second sees none, and only the first has the flag.
func TestAVirtualMailboxHasRecentOfItsOwn(t *testing.T) {
	a, ard, login := recentServer(t)
	if got := recentOf(command(t, a, ard, "a2", "SELECT Virtual/All")); got != 2 {
		t.Errorf("first virtual SELECT: RECENT %d, want 2", got)
	}
	b, brd := login()
	if got := recentOf(command(t, b, brd, "b2", "SELECT Virtual/All")); got != 0 {
		t.Errorf("second virtual SELECT: RECENT %d, want 0", got)
	}
	if got := flaggedRecent(command(t, a, ard, "a3", "FETCH 1:* (FLAGS)")); got != "1,2" {
		t.Errorf("first session FETCH: \\Recent on %q, want 1,2", got)
	}
	if got := flaggedRecent(command(t, b, brd, "b3", "FETCH 1:* (FLAGS)")); got != "" {
		t.Errorf("second session FETCH: \\Recent on %q, want none", got)
	}
}

// "+" before a folder takes that folder's \Recent with the virtual SELECT; a
// folder named without it keeps them.
func TestAPlusFolderGivesUpItsRecent(t *testing.T) {
	cases := []struct {
		config string
		want   int
	}{
		{"+INBOX\n", 0},
		{"INBOX\n", 2},
	}
	for _, tc := range cases {
		t.Run(strings.TrimSpace(tc.config), func(t *testing.T) {
			a, ard, login := recentServerWith(t, tc.config)
			command(t, a, ard, "a2", "SELECT Virtual/All")
			b, brd := login()
			if got := recentOf(command(t, b, brd, "b2", "SELECT INBOX")); got != tc.want {
				t.Errorf("SELECT INBOX after the virtual SELECT: RECENT %d, want %d", got, tc.want)
			}
		})
	}
}

// EXAMINE and STATUS of a virtual mailbox read its \Recent and leave it.
func TestExamineAndStatusLeaveVirtualRecent(t *testing.T) {
	a, ard, login := recentServer(t)
	if got := recentOf(command(t, a, ard, "a2", "EXAMINE Virtual/All")); got != 2 {
		t.Errorf("virtual EXAMINE: RECENT %d, want 2", got)
	}
	b, brd := login()
	for _, l := range command(t, b, brd, "b2", "STATUS Virtual/All (RECENT)") {
		if strings.HasPrefix(l, "* STATUS") && !strings.Contains(l, "RECENT 2") {
			t.Errorf("virtual STATUS answered %q, want RECENT 2", l)
		}
	}
	if got := recentOf(command(t, b, brd, "b3", "SELECT Virtual/All")); got != 2 {
		t.Errorf("virtual SELECT after EXAMINE and STATUS: RECENT %d, want 2", got)
	}
}

// A rev2 session selecting a virtual mailbox sees no \Recent but takes it.
func TestARev2SessionTakesVirtualRecentUnseen(t *testing.T) {
	a, ard, login := recentServer(t)
	command(t, a, ard, "a2", "ENABLE IMAP4rev2")
	if got := recentOf(command(t, a, ard, "a3", "SELECT Virtual/All")); got != -1 {
		t.Errorf("rev2 virtual SELECT: RECENT %d, want no RECENT response", got)
	}
	if got := flaggedRecent(command(t, a, ard, "a4", "FETCH 1:* (FLAGS)")); got != "" {
		t.Errorf("rev2 virtual FETCH: \\Recent on %q, want none", got)
	}
	b, brd := login()
	if got := recentOf(command(t, b, brd, "b2", "SELECT Virtual/All")); got != 0 {
		t.Errorf("virtual SELECT after a rev2 one: RECENT %d, want 0", got)
	}
}

// Mail arriving in a "+" folder while the virtual mailbox is selected is taken
// at the next poll, as the folder is held open with the selection.
func TestAPlusFolderGivesUpArrivalsToo(t *testing.T) {
	a, ard, login := recentServerWith(t, "+INBOX\n")
	command(t, a, ard, "a2", "SELECT Virtual/All")
	b, brd := login()
	appendInbox(t, b, brd, "b2", "three")
	command(t, a, ard, "a3", "NOOP")
	c, crd := login()
	if got := recentOf(command(t, c, crd, "c2", "SELECT INBOX")); got != 0 {
		t.Errorf("SELECT INBOX after the poll: RECENT %d, want 0", got)
	}
}
