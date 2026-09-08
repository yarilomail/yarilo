package maildir

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The debug rows exist to pin a lost list row to a moment and a holder, so each
// one has to carry both: without the owner a line names no writer (#1739).
func TestTheDebugRowNamesTheWriterAndBothSides(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	box := openTestUser(t, t.TempDir())
	u := box.(*userMailbox)
	body := "From: a@b\r\n\r\nx\r\n"
	name, _, _, err := u.Save("INBOX", strings.NewReader(body), 1, int64(len(body)), nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.AssignUID("INBOX", name, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := mailbox.MessagePath(box, "INBOX", &mailbox.MessageMeta{UID: 1}); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	for _, want := range []string{
		`"msg":"maildir: uidlist written"`, `"site":"assign"`, `"owner":`,
		`"rows_before":0`, `"rows_after":1`, `"mtime_before":`, `"mtime_after":`,
		`"msg":"maildir: uidlist read"`, `"from":"cache"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the debug rows do not carry %s", want)
		}
	}

	// A handle with nothing cached reads the file, and says so: telling the two
	// apart is the point, since a stale snapshot reads as a missing row.
	buf.Reset()
	fresh := openTestUser(t, homeOf(t, u)).(*userMailbox)
	if _, err := mailbox.MessagePath(fresh, "INBOX", &mailbox.MessageMeta{UID: 1}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"from":"disk"`) {
		t.Errorf("a cold handle did not report a disk read: %s", buf.String())
	}
}

// homeOf is the user's home, so a second handle can open the same tree.
func homeOf(t *testing.T, u *userMailbox) string {
	t.Helper()
	return u.home
}

// Nothing is said at the default level: the rows cost an operator's choice, not
// every deployment's log volume.
func TestTheDebugRowsAreSilentAtInfo(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(prev)

	box := openTestUser(t, t.TempDir())
	u := box.(*userMailbox)
	body := "From: a@b\r\n\r\nx\r\n"
	name, _, _, err := u.Save("INBOX", strings.NewReader(body), 1, int64(len(body)), nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.AssignUID("INBOX", name, 1); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "uidlist") {
		t.Errorf("the debug rows spoke at info level: %s", buf.String())
	}
}
