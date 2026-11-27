package maildir

import (
	"io"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// RawRecord returns the bytes on disk; OpenRecord serves the same body CRLF.
func TestRawRecordKeepsBareLF(t *testing.T) {
	box, _ := newBox(t, "u@x")
	box.Init() //nolint:errcheck
	body := "From: a@b\n\nhello\n"
	name, _, _, err := box.Save("INBOX", strings.NewReader(body), 1, int64(len(body)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.AssignUID("INBOX", name, 1); err != nil {
		t.Fatal(err)
	}
	u := box
	m := &mailbox.MessageMeta{UID: 1}

	raw, err := u.RawRecord("INBOX", m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(io.NewSectionReader(raw, 0, raw.Size()))
	_ = raw.Close()
	if err != nil || string(got) != body {
		t.Fatalf("raw %q err %v, want %q", got, err, body)
	}

	rc, err := u.OpenRecord("INBOX", m)
	if err != nil {
		t.Fatal(err)
	}
	served, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(served) != strings.ReplaceAll(body, "\n", "\r\n") {
		t.Fatalf("served %q: the row no longer tells raw from served", served)
	}
}
