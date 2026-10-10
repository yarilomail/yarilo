package maildir

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Every way a read can answer names itself, with the stamp of the file it
// stands for (#2183).
func TestReadUIDListFromNamesItsSource(t *testing.T) {
	box, _, _ := recSetup(t)
	path := filepath.Join(box.controlFolderPath("INBOX"), UIDListFileName)
	stampNow := func() listStamp {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return stampOf(fi)
	}
	check := func(want string, st listStamp) {
		t.Helper()
		_, read, err := box.readUIDListFrom("INBOX")
		if err != nil {
			t.Fatal(err)
		}
		if read.from != want {
			t.Errorf("from = %q, want %q", read.from, want)
		}
		if read.stamp.inode() != st.inode() || read.stamp.size != st.size || !read.stamp.mtime.Equal(st.mtime) {
			t.Errorf("%s: stamp = %+v, want %+v", want, read.stamp, st)
		}
	}

	if _, err := box.AssignUID("INBOX", "1700000000.M1P1.h,S=3", 1); err != nil {
		t.Fatal(err)
	}
	// Another process's first read: nothing kept, the whole file parsed.
	box = New().OpenUser(&mailbox.UserInfo{Username: box.username, Home: box.home}).(*userMailbox)
	check("file", stampNow())
	check("cache", stampNow())

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("2 :1700000001.M2P1.h,S=3\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	check("tail", stampNow())

	box.folderCacheFor("INBOX").markChecked()
	check("window", stampNow())
}

// The rebind line carries where the map came from, the list's stamp and the
// record's modseq: without them it says what was done, not why (#2183).
func TestTheRebindLineSaysWhereTheListCameFrom(t *testing.T) {
	box, idx, folder := recSetup(t)
	b := mailboxbase.Open(box, idx)
	const body = "From: a@b\r\nSubject: relink\r\n\r\nbody\r\n"
	name, vsize, guid, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.RecordSaved(folder, "INBOX", name, &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid}); err != nil {
		t.Fatal(err)
	}
	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("records: %d, %v", len(msgs), err)
	}

	// The row alone goes: the header stays, so the read is of a real file.
	path := filepath.Join(box.controlFolderPath("INBOX"), UIDListFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	header, _, _ := strings.Cut(string(raw), "\n")
	if err := os.WriteFile(path+".new", []byte(header+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	want := stampOf(fi)

	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(prev)
	if _, err := box.ReconcileIndex(b, idx, folder); err != nil {
		t.Fatal(err)
	}

	var line map[string]any
	for _, l := range strings.Split(logged.String(), "\n") {
		if strings.Contains(l, "the list stopped naming this record") {
			if err := json.Unmarshal([]byte(l), &line); err != nil {
				t.Fatal(err)
			}
		}
	}
	if line == nil {
		t.Fatalf("no rebind line in %q", logged.String())
	}
	for k, v := range map[string]float64{
		"modseq":     float64(msgs[0].ModSeq),
		"list_ino":   float64(want.inode()),
		"list_size":  float64(want.size),
		"list_mtime": float64(want.mtime.UnixNano()),
	} {
		if got, _ := line[k].(float64); got != v {
			t.Errorf("%s = %v, want %v", k, line[k], v)
		}
	}
	// The pass's own scan read this file first, so the map comes kept; the
	// stamp above is what ties it to the file that lost the row.
	if from := line["list_from"]; from != "cache" && from != "file" {
		t.Errorf("list_from = %v, want cache or file", from)
	}
}
