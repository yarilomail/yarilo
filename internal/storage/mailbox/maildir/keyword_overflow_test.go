package maildir_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// A name carries 26 keyword letters at most: the 27th keyword gets none, the
// write still lands with the 26, and the miss is logged rather than dropped.
func TestTheTwentySeventhKeywordGetsNoLetter(t *testing.T) {
	box := maildir.New().OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: t.TempDir(), Driver: "maildir"})
	defer box.Close() //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	name, _, _, err := box.Save("INBOX", strings.NewReader("From: a@b\r\n\r\nx\r\n"), 1, 0, nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if name, err = mailbox.Driver(box).(mailbox.UIDNamer).AssignUID("INBOX", name, 1); err != nil {
		t.Fatal(err)
	}
	var kws []string
	for i := range 27 {
		kws = append(kws, fmt.Sprintf("$K%02d", i))
	}
	var log bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&log, nil)))
	defer slog.SetDefault(prev)

	got, err := mailbox.Driver(box).(mailbox.FlagWriter).WriteFlags("INBOX", name, nil, kws)
	if err != nil {
		t.Fatalf("write flags: %v", err)
	}
	info := got[strings.Index(got, ":2,")+3:]
	if n := strings.IndexFunc(info, func(r rune) bool { return r < 'a' || r > 'z' }); n != -1 || len(info) != 26 {
		t.Errorf("the name carries %q, want the 26 letters a-z", info)
	}
	if !strings.Contains(log.String(), "no free keyword letter") || !strings.Contains(log.String(), "$K26") {
		t.Errorf("the 27th keyword's miss was not logged: %s", log.String())
	}
}
