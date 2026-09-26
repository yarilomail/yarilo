package imap_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/virtual"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// saveRaw files a message as written, for rows that need their own headers.
func saveRaw(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex, folder string, uid uint32, raw string, day int) {
	t.Helper()
	if folder != "INBOX" {
		box.Create(folder) //nolint:errcheck
	}
	name, vsize, guid, err := box.Save(folder, strings.NewReader(raw), uid, int64(len(raw)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	f, err := ui.OpenFolder(folder, 0)
	if err != nil {
		t.Fatal(err)
	}
	meta := &mailbox.MessageMeta{
		UID: uid, Size: uint32(len(raw)), VSize: vsize, GUID: guid,
		InternalDate: time.Date(2026, 3, day, 0, 0, 0, 0, time.UTC),
	}
	if err := mailboxbase.NameSaved(box, folder, name, meta); err != nil {
		t.Fatal(err)
	}
	if err := ui.AppendMessage(f.ID, meta); err != nil {
		t.Fatal(err)
	}
}

func tagged(t *testing.T, conn interface{ Write([]byte) (int, error) }, rd interface{ ReadString(byte) (string, error) }, tag, cmd string) []string {
	t.Helper()
	fmt.Fprintf(conn, "%s %s\r\n", tag, cmd)
	var lines []string
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		lines = append(lines, line)
		if strings.HasPrefix(line, tag+" ") {
			return lines
		}
	}
}

func last(lines []string) string { return lines[len(lines)-1] }

const saveConfig = "INBOX\nSaved\n!Saved\n"

func seedSaved(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
	saveInto(t, box, ui, "INBOX", 1, "in inbox", nil)
	box.Create("Saved") //nolint:errcheck
	if _, err := ui.OpenFolder("Saved", 0); err != nil {
		t.Fatal(err)
	}
}

// APPEND into a virtual mailbox stores in its "!" folder with the flags given,
// without APPENDUID: the uid is that folder's (virtual-save.c:78-117, :70).
func TestAppendIntoAVirtualMailboxStoresInItsSaveFolder(t *testing.T) {
	conn, rd := virtualServer(t, map[string]string{"All": saveConfig}, seedSaved)
	if got := existsCount(t, conn, rd, "a2", "Virtual/All"); got != 1 {
		t.Fatalf("EXISTS = %d, want 1", got)
	}
	raw := "Subject: appended\r\nFrom: a@test\r\n\r\nbody\r\n"
	fmt.Fprintf(conn, "a3 APPEND Virtual/All (\\Flagged) {%d+}\r\n%s\r\n", len(raw), raw)
	if line := readTagged(t, rd, "a3"); !strings.HasPrefix(line, "a3 OK") || strings.Contains(line, "APPENDUID") {
		t.Errorf("APPEND answered %q, want OK without APPENDUID", line)
	}
	if got := existsCount(t, conn, rd, "a4", "Saved"); got != 1 {
		t.Fatalf("Saved holds %d messages, want the appended one", got)
	}
	if f := flagsOf(t, fetchLine(t, conn, rd, "a5", "FETCH 1 (FLAGS)")); !strings.Contains(f, `\Flagged`) {
		t.Errorf("the stored message has FLAGS (%s), want the \\Flagged it was appended with", f)
	}
	if got := existsCount(t, conn, rd, "a6", "Virtual/All"); got != 2 {
		t.Errorf("Virtual/All EXISTS = %d after the append, want 2", got)
	}
}

// Without a "!" line a virtual mailbox stores nothing, and says so as a refusal,
// not as a server fault (#2047).
func TestAVirtualMailboxWithoutSaveFolderRefusesWrites(t *testing.T) {
	conn, rd := virtualServer(t, map[string]string{"All": "INBOX\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "one", nil)
		})
	existsCount(t, conn, rd, "a2", "INBOX")
	raw := "Subject: x\r\n\r\nbody\r\n"
	fmt.Fprintf(conn, "a3 APPEND Virtual/All {%d+}\r\n%s\r\n", len(raw), raw)
	for _, tc := range []struct{ what, answer string }{
		{"APPEND", readTagged(t, rd, "a3")},
		{"COPY", last(tagged(t, conn, rd, "a4", "COPY 1 Virtual/All"))},
		{"DELETE", last(tagged(t, conn, rd, "a5", "DELETE Virtual/All"))},
		{"RENAME", last(tagged(t, conn, rd, "a6", "RENAME Virtual/All Virtual/Other"))},
	} {
		if !strings.Contains(tc.answer, "NO [CANNOT]") {
			t.Errorf("%s answered %q, want NO [CANNOT]", tc.what, tc.answer)
		}
	}
}

// COPY into a virtual mailbox copies into its "!" folder, without COPYUID.
func TestCopyIntoAVirtualMailboxStoresInItsSaveFolder(t *testing.T) {
	conn, rd := virtualServer(t, map[string]string{"All": saveConfig}, seedSaved)
	existsCount(t, conn, rd, "a2", "INBOX")
	if line := last(tagged(t, conn, rd, "a3", "COPY 1 Virtual/All")); !strings.HasPrefix(line, "a3 OK") || strings.Contains(line, "COPYUID") {
		t.Errorf("COPY answered %q, want OK without COPYUID", line)
	}
	if got := existsCount(t, conn, rd, "a4", "Saved"); got != 1 {
		t.Errorf("Saved holds %d messages, want the copy", got)
	}
	if got := existsCount(t, conn, rd, "a5", "INBOX"); got != 1 {
		t.Errorf("INBOX holds %d messages, want its own kept", got)
	}
}

// MOVE into a virtual mailbox takes the message from its source only once it
// is stored; refused, it takes nothing.
func TestMoveIntoAVirtualMailboxLosesTheSourceOnlyWhenStored(t *testing.T) {
	for _, tc := range []struct {
		name, config   string
		ok             bool
		inbox, savedTo int
	}{
		{"stored in the save folder", saveConfig, true, 0, 1},
		{"no save folder: refused, source kept", "INBOX\nSaved\n", false, 1, 0},
		{"save folder missing: refused, source kept", "INBOX\n!Gone\n", false, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, rd := virtualServer(t, map[string]string{"All": tc.config}, seedSaved)
			existsCount(t, conn, rd, "a2", "INBOX")
			lines := tagged(t, conn, rd, "a3", "MOVE 1 Virtual/All")
			answer := last(lines)
			if tc.ok && (!strings.HasPrefix(answer, "a3 OK") || len(linesWith(lines, "COPYUID")) != 0) {
				t.Errorf("MOVE answered %v, want OK without COPYUID", lines)
			}
			if !tc.ok && !strings.Contains(answer, "NO [CANNOT]") {
				t.Errorf("MOVE answered %q, want NO [CANNOT]", answer)
			}
			if got := existsCount(t, conn, rd, "a4", "INBOX"); got != tc.inbox {
				t.Errorf("INBOX holds %d, want %d", got, tc.inbox)
			}
			if got := existsCount(t, conn, rd, "a5", "Saved"); got != tc.savedTo {
				t.Errorf("Saved holds %d, want %d", got, tc.savedTo)
			}
		})
	}
}

// A configuration the server cannot read is the server's fault: the client is
// told NO [SERVERBUG], the log names the line (virtual-config.c:516, #2048).
func TestAnInvalidVirtualConfigurationIsTheServersFault(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	conn, rd := virtualServer(t, map[string]string{"All": "INBOX\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveInto(t, box, ui, "INBOX", 1, "one", nil)
		})
	existsCount(t, conn, rd, "a2", "Virtual/All")
	// The operator edits the file of a mailbox already in use.
	path := filepath.Join(lastVirtualHome, "virtual", "All", virtual.ConfigFileName)
	if err := os.WriteFile(path, []byte("INBOX\n  nosuchkey\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if line := last(tagged(t, conn, rd, "a3", "SELECT Virtual/All")); !strings.Contains(line, "NO [SERVERBUG]") {
		t.Errorf("SELECT answered %q, want NO [SERVERBUG]", line)
	}
	if !strings.Contains(logs.String(), "virtual: line 2:") || !strings.Contains(logs.String(), virtual.ConfigFileName) {
		t.Errorf("the log names no file and line: %q", logs.String())
	}
}

// SORT and THREAD in a virtual mailbox order by what the copies say: their
// headers and dates, read from the folders they live in (virtual-mail.c:502).
func TestSortAndThreadReadTheCopies(t *testing.T) {
	conn, rd := virtualServer(t, map[string]string{"All": "INBOX\nArchive\n"},
		func(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
			saveRaw(t, box, ui, "INBOX", 1, "Subject: charlie\r\nMessage-ID: <root@x>\r\nDate: Sun, 1 Mar 2026 10:00:00 +0000\r\n\r\n"+strings.Repeat("root, the longest body of the three ", 8)+"\r\n", 3)
			saveRaw(t, box, ui, "INBOX", 2, "Subject: alpha\r\nMessage-ID: <other@x>\r\nDate: Mon, 2 Mar 2026 10:00:00 +0000\r\n\r\nx\r\n", 1)
			saveRaw(t, box, ui, "Archive", 1, "Subject: Re: charlie\r\nMessage-ID: <reply@x>\r\nIn-Reply-To: <root@x>\r\nReferences: <root@x>\r\nDate: Tue, 3 Mar 2026 10:00:00 +0000\r\n\r\nreply body\r\n", 2)
		})
	if got := existsCount(t, conn, rd, "a2", "Virtual/All"); got != 3 {
		t.Fatalf("EXISTS = %d, want 3", got)
	}
	for _, tc := range []struct{ key, want string }{
		{"SUBJECT", "* SORT 2 1 3"},
		{"ARRIVAL", "* SORT 2 3 1"},
		{"SIZE", "* SORT 2 3 1"},
		{"REVERSE DATE", "* SORT 3 2 1"},
	} {
		if got := linesWith(tagged(t, conn, rd, "s", "SORT ("+tc.key+") UTF-8 ALL"), "* SORT"); len(got) != 1 || got[0] != tc.want {
			t.Errorf("SORT (%s) answered %v, want %s", tc.key, got, tc.want)
		}
	}
	got := linesWith(tagged(t, conn, rd, "th", "THREAD REFERENCES UTF-8 ALL"), "* THREAD")
	if len(got) != 1 || !strings.Contains(got[0], "(1 3)") {
		t.Errorf("THREAD answered %v, want the reply in Archive threaded under its root in INBOX", got)
	}
}
