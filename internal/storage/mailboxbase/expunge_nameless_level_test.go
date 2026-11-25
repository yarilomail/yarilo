package mailboxbase_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// An expunge of a message whose file another session already removed drops
// the record, as the reference's maildir expunge does on ENOENT, and logs it
// at Debug: the race is ordinary, not a fault.
func TestAnExpungeOfAFileAlreadyGoneDropsTheRecordQuietly(t *testing.T) {
	for _, driver := range []string{"maildir", "sdbox"} {
		t.Run(driver, func(t *testing.T) {
			box, f := boxOn(t, driver)
			m := deliverOne(t, box, f)
			if err := box.RemoveMessage("INBOX", m); err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			_, failed, err := box.ExpungeMarked(f, "INBOX", []*mailbox.MessageMeta{m})
			slog.SetDefault(prev)
			if err != nil || failed != 0 {
				t.Fatalf("expunge: failed %d, %v", failed, err)
			}
			if msgs, _ := box.Messages(f.ID, mailbox.SeqSet{{From: m.UID, To: m.UID}}); len(msgs) != 0 {
				t.Errorf("the record stayed after the expunge: %v", msgs)
			}
			for _, l := range strings.Split(buf.String(), "\n") {
				if strings.Contains(l, "the record named no file") && !strings.Contains(l, "level=DEBUG") {
					t.Errorf("a file already gone logged above Debug: %s", l)
				}
			}
		})
	}
}
