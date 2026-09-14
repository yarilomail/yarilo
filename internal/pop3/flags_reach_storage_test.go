package pop3

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// maildirStand delivers one message into a real maildir and returns the server
// options a session runs on, plus the folder the file lives in.
func maildirStand(t *testing.T, opts func(*Options)) (Options, string) {
	return maildirStandN(t, opts, 1)
}

// maildirStandN is the same with n messages, for a row that has to tell the
// session's own reads from the mailbox's.
func maildirStandN(t *testing.T, opts func(*Options), n int) (Options, string) {
	t.Helper()
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@x", Home: home, Driver: "maildir"}
	mb := maildir.New()
	box := mb.OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	idx := fileindex.New().OpenUser(info)
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		raw := fmt.Sprintf("From: a@b\r\nSubject: msg %d\r\n\r\nbody\r\n", i+1)
		saved, vsize, guid, serr := box.Save("INBOX", strings.NewReader(raw), 0, int64(len(raw)), nil, nil, [16]byte{})
		if serr != nil {
			t.Fatal(serr)
		}
		m := &mailbox.MessageMeta{Size: uint32(len(raw)), VSize: vsize, GUID: guid}
		if err := mailboxbase.RecordSaved(idx, box, f.ID, "INBOX", saved, m); err != nil {
			t.Fatal(err)
		}
	}
	box.Close() //nolint:errcheck
	idx.Close() //nolint:errcheck

	o := newTestOpts(&mockAuth{users: map[string]string{"u@x": "p"}, home: home}, mb, fileindex.New())
	if opts != nil {
		opts(&o)
	}
	return o, filepath.Join(home, "Maildir", "cur")
}

// curName is the one file in cur/, which is where a maildir keeps its flags.
func curName(t *testing.T, cur string) string {
	t.Helper()
	entries, err := os.ReadDir(cur)
	if err != nil {
		t.Fatalf("read cur: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("cur holds %d files, want 1: %v", len(entries), entries)
	}
	return entries[0].Name()
}

// trailersBySubject maps each message's subject to the flag trailer its file
// carries, so a row can name which message it means.
func trailersBySubject(t *testing.T, cur string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(cur)
	if err != nil {
		t.Fatalf("read cur: %v", err)
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		body, rerr := os.ReadFile(filepath.Join(cur, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		subject := ""
		for _, line := range strings.Split(string(body), "\r\n") {
			if rest, ok := strings.CutPrefix(line, "Subject: "); ok {
				subject = rest
				break
			}
		}
		out[subject] = flagTrailer(e.Name())
	}
	return out
}

func flagTrailer(name string) string {
	if i := strings.Index(name, ":2,"); i >= 0 {
		return name[i+3:]
	}
	return ""
}

// A flag a POP3 session sets reaches the name the maildir keeps it in: a flag
// that stopped at our index is one the store does not have (#1780).
func TestAPOP3FlagReachesTheMaildirName(t *testing.T) {
	cases := []struct {
		name    string
		opts    func(*Options)
		script  []string
		want    string // letters the trailer must carry
		absent  string // letters it must not
		removed bool   // the file is gone instead
	}{
		{
			name:   "RETR marks the name seen",
			script: []string{"RETR 1", "QUIT"},
			want:   "S",
		},
		{
			name:   "DELE with delete_type flag marks the name",
			opts:   func(o *Options) { o.DeleteType = "flag"; o.DeletedFlag = `\Deleted` },
			script: []string{"DELE 1", "QUIT"},
			want:   "T",
		},
		{
			name:    "DELE with delete_type expunge removes the file",
			script:  []string{"DELE 1", "QUIT"},
			removed: true,
		},
		{
			name:   "RSET before QUIT leaves the name alone",
			opts:   func(o *Options) { o.DeleteType = "flag"; o.DeletedFlag = `\Deleted` },
			script: []string{"RETR 1", "DELE 1", "RSET", "QUIT"},
			absent: "ST",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts, cur := maildirStand(t, c.opts)
			before := curName(t, cur)
			runPOP3(t, opts, c.script)

			if c.removed {
				entries, _ := os.ReadDir(cur)
				if len(entries) != 0 {
					t.Errorf("cur still holds %d files after an expunging DELE", len(entries))
				}
				return
			}
			got := flagTrailer(curName(t, cur))
			for _, r := range c.want {
				if !strings.ContainsRune(got, r) {
					t.Errorf("the name trailer is %q, want %q in it (was %q)", got, string(r), flagTrailer(before))
				}
			}
			for _, r := range c.absent {
				if strings.ContainsRune(got, r) {
					t.Errorf("the name trailer is %q and carries %q, which nothing committed", got, string(r))
				}
			}
		})
	}
}

// LAST after RSET reports zero, so the clear covers the whole mailbox and not
// only what this session read (RFC 1460).
func TestRsetClearsSeenLeftByAnEarlierSession(t *testing.T) {
	opts, cur := maildirStand(t, func(o *Options) { o.EnableLast = true })

	// An earlier session read the message.
	runPOP3(t, opts, []string{"RETR 1", "QUIT"})
	if got := flagTrailer(curName(t, cur)); !strings.Contains(got, "S") {
		t.Fatalf("setup: the trailer is %q, want S from the first session", got)
	}

	// A new session resets: LAST must answer 0, and the name must lose S.
	lines := runPOP3(t, opts, []string{"RSET", "LAST", "QUIT"})
	if got := flagTrailer(curName(t, cur)); strings.Contains(got, "S") {
		t.Errorf("the trailer is %q after RSET; the clear only covered this session", got)
	}
	if last := lines["LAST"]; !strings.HasPrefix(last, "+OK 0") {
		t.Errorf("LAST after RSET is %q, want +OK 0", last)
	}
}

// runPOP3 logs in, runs the commands and returns each one's first reply line.
func runPOP3(t *testing.T, opts Options, cmds []string) map[string]string {
	t.Helper()
	c, br := newPOP3Session(t, opts)
	login(t, c, br, "u@x", "p")
	out := make(map[string]string, len(cmds))
	for _, cmd := range cmds {
		send(t, c, cmd)
		line := readline(t, br)
		out[cmd] = line
		if strings.HasPrefix(cmd, "RETR") && strings.HasPrefix(line, "+OK") {
			for {
				l := readline(t, br)
				if strings.TrimRight(l, "\r\n") == "." {
					break
				}
			}
		}
	}
	// The session applies everything on QUIT; give the server the moment it
	// needs to finish writing before the row reads the directory.
	time.Sleep(50 * time.Millisecond)
	c.Close() //nolint:errcheck
	return out
}

// RSET clears \Seen across the mailbox, not only what this session read: LAST
// has to answer zero, and a message an earlier session read is still seen.
func TestRsetClearsSeenAcrossTheMailbox(t *testing.T) {
	opts, cur := maildirStandN(t, func(o *Options) { o.EnableLast = true }, 2)

	// An earlier session reads the first message and commits it.
	runPOP3(t, opts, []string{"RETR 1", "QUIT"})
	if got := trailersBySubject(t, cur)["msg 1"]; !strings.Contains(got, "S") {
		t.Fatalf("setup: msg 1 trailer is %q, want S from the first session", got)
	}

	// A new session reads the second and resets. Both must come back unseen.
	lines := runPOP3(t, opts, []string{"RETR 2", "RSET", "LAST", "QUIT"})
	trailers := trailersBySubject(t, cur)
	for _, subject := range []string{"msg 1", "msg 2"} {
		if strings.Contains(trailers[subject], "S") {
			t.Errorf("%s keeps %q after RSET; the clear only covered this session's reads",
				subject, trailers[subject])
		}
	}
	if last := lines["LAST"]; !strings.HasPrefix(last, "+OK 0") {
		t.Errorf("LAST after RSET is %q, want +OK 0", last)
	}
}

// A name the store would not take is counted apart from a record the index
// refused: the second is the ordinary failure, the first is #1780 itself.
func TestAStoreThatRefusesTheNameIsCountedApart(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@x", Home: home, Driver: "maildir"}
	box := maildir.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	idx := fileindex.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	raw := "From: a@b\r\nSubject: one\r\n\r\nbody\r\n"
	saved, vsize, guid, err := box.Save("INBOX", strings.NewReader(raw), 0, int64(len(raw)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{Size: uint32(len(raw)), VSize: vsize, GUID: guid}
	if err := mailboxbase.RecordSaved(idx, box, f.ID, "INBOX", saved, m); err != nil {
		t.Fatal(err)
	}
	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("setup: %d records, %v", len(msgs), err)
	}

	// The rename has nowhere to go: cur/ is readable but not writable.
	cur := filepath.Join(home, "Maildir", "cur")
	if err := os.Chmod(cur, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(cur, 0o700) }) //nolint:errcheck

	s := &session{
		box:      mailboxbase.Open(box, idx),
		folder:   f,
		msgs:     msgs,
		userInfo: info,
		srv:      &Server{opts: Options{}},
	}
	got := s.writeFlagBatch(map[uint32][]string{msgs[0].UID: {`\Seen`}})
	if got.storeRefused != 1 {
		t.Errorf("store_refused = %d, want 1: the name never took the flag", got.storeRefused)
	}
	if got.indexRefused != 0 {
		t.Errorf("index_refused = %d, want 0: the record was written", got.indexRefused)
	}
	if got.applied != 1 {
		t.Errorf("applied = %d, want 1", got.applied)
	}
}
