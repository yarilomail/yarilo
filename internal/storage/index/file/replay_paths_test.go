package file

import (
	"bytes"
	"math/rand/v2"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// replaySeed is YARILO_TEST_SEED when set, so a failure in the gate replays
// here; a fresh one otherwise, named in every failure.
func replaySeed(t *testing.T) uint64 {
	if v := os.Getenv("YARILO_TEST_SEED"); v != "" {
		s, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	return uint64(time.Now().UnixNano())
}

// viewRecords is a reader's view of the folder through openView: its
// messages and its records as bytes, whichever form it holds them in.
func viewRecords(t *testing.T, h mailbox.UserIndex, id uint64) ([]*mailbox.MessageMeta, []byte, bool) {
	t.Helper()
	msgs, err := h.GetMessages(id, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	fs, err := h.(*userHandle).ui.state(id)
	if err != nil {
		t.Fatal(err)
	}
	v, release, err := fs.openView()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := v.loadRecords(); err != nil {
		t.Fatal(err)
	}
	if v.recs != nil {
		return msgs, v.recs.Bytes(), true
	}
	layout, err := mailindex.ComputeRecordLayout(v.file.Extensions)
	if err != nil {
		t.Fatal(err)
	}
	r, err := mailindex.ChunksFromRecords(v.file.Records, layout)
	if err != nil {
		t.Fatal(err)
	}
	return msgs, r.Bytes(), false
}

// The one journal replay gives the same records through a view's chunks as
// through decoded records, after every step of a random run of appends, flag
// and keyword changes, expunges, a new record extension (where a chunked view
// turns to decoded records) and base rewrites (where it turns back).
func TestTheReplayGivesTheSameRecordsInBothForms(t *testing.T) {
	seed := replaySeed(t)
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	home := movedFolder(t, 3*mailindex.ChunkRecords)
	withoutExtension(t, home, extNameMdbox)
	w, wf := acceptOpen(t, home)
	defer w.Close() //nolint:errcheck
	chunked, cf := acceptOpen(t, home)
	defer chunked.Close() //nolint:errcheck
	decoded, df := acceptOpen(t, home)
	defer decoded.Close() //nolint:errcheck

	uids := func() []uint32 {
		msgs, err := w.GetMessages(wf.ID, mailbox.SeqSet{})
		if err != nil {
			t.Fatal(err)
		}
		out := make([]uint32, len(msgs))
		for i, m := range msgs {
			out[i] = m.UID
		}
		return out
	}
	introduced := false
	// The forms the chunked reader's view took: chunks before the extension,
	// decoded records after it, chunks again after the next base rewrite.
	var sawChunks, sawDecoded, sawChunksAgain bool
	for step := 0; step < 300; step++ {
		live := uids()
		op := rng.IntN(20)
		var what string
		switch {
		case op < 6 && len(live) > 0:
			uid := live[rng.IntN(len(live))]
			what = "flags of " + strconv.Itoa(int(uid))
			if err := w.UpdateFlags(wf.ID, uid, []string{`\Seen`, `\Flagged`}[rng.IntN(2):], []string{"$A", "$B", "$C"}[rng.IntN(3):]); err != nil {
				t.Fatalf("seed %d step %d %s: %v", seed, step, what, err)
			}
		case op < 11:
			uid, err := w.AllocateUID(wf.ID)
			if err != nil {
				t.Fatal(err)
			}
			m := &mailbox.MessageMeta{UID: uid, Size: 100 + uint32(step), VSize: 120, InternalDate: time.Unix(1700000000+int64(step), 0)}
			if !introduced && step > 100 {
				m.MapUID, introduced = 7, true // the mdbox extension enters the journal
			}
			what = "append " + strconv.Itoa(int(uid))
			if err := w.AppendMessage(wf.ID, m); err != nil {
				t.Fatalf("seed %d step %d %s: %v", seed, step, what, err)
			}
		case op < 15 && len(live) > 0:
			uid := live[rng.IntN(len(live))]
			what = "expunge " + strconv.Itoa(int(uid))
			tx, err := w.Begin(wf.ID)
			if err != nil {
				t.Fatal(err)
			}
			tx.Expunge(uid)
			if _, err := tx.Commit(); err != nil {
				t.Fatalf("seed %d step %d %s: %v", seed, step, what, err)
			}
		case op < 16:
			what = "base rewrite"
			if err := w.OptimizeIndex(wf.ID); err != nil {
				t.Fatalf("seed %d step %d %s: %v", seed, step, what, err)
			}
		default:
			what = "a read"
		}
		cm, cb, inChunks := viewRecords(t, chunked, cf.ID)
		decodedImages = true
		dm, db, _ := viewRecords(t, decoded, df.ID)
		decodedImages = false
		switch {
		case inChunks && !introduced:
			sawChunks = true
		case !inChunks && introduced:
			sawDecoded = true
		case inChunks && sawDecoded:
			sawChunksAgain = true
		}
		ch, dh := viewHeader(t, chunked, cf.ID), viewHeader(t, decoded, df.ID)
		if ch != dh {
			t.Fatalf("seed %d step %d after %s: the chunked view's header %+v, the decoded one's %+v", seed, step, what, ch, dh)
		}
		if !reflect.DeepEqual(cm, dm) {
			t.Fatalf("seed %d step %d after %s: the chunked view answers %d messages, the decoded one %d, and they differ",
				seed, step, what, len(cm), len(dm))
		}
		if wb := writerRecords(t, w, wf.ID); !bytes.Equal(wb, db) {
			t.Fatalf("seed %d step %d after %s: the writer's records differ from what the journal gives a reader (%d and %d bytes)",
				seed, step, what, len(wb), len(db))
		}
		if !bytes.Equal(cb, db) {
			t.Fatalf("seed %d step %d after %s: the records differ as bytes (%d and %d)", seed, step, what, len(cb), len(db))
		}
	}
	if !introduced || !sawChunks || !sawDecoded || !sawChunksAgain {
		t.Fatalf("seed %d: the run did not cross both ways: extension %v, chunks %v, decoded %v, chunks again %v",
			seed, introduced, sawChunks, sawDecoded, sawChunksAgain)
	}
}

// withoutExtension rewrites the folder's base with one record extension
// fewer, as a folder written before it existed has it.
func withoutExtension(t *testing.T, home, name string) {
	t.Helper()
	path := indexPathFor(home)
	f, err := mailindex.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var exts []mailindex.Extension
	for _, e := range f.Extensions {
		if e.Name != name {
			exts = append(exts, e)
		}
	}
	layout, err := mailindex.ComputeRecordLayout(exts)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range f.Records {
		delete(r.Ext, name)
	}
	hdr := f.Header
	hdr.RecordSize = layout.RecordSize
	extBytes, err := mailindex.EncodeExtHeaders(layout.Extensions)
	if err != nil {
		t.Fatal(err)
	}
	hdr.HeaderSize = uint32(hdr.BaseHeaderSize) + uint32(len(extBytes))
	if _, err := mailindex.Recreate(mailindex.RecreateInput{Path: path, Header: hdr, Extensions: layout.Extensions, Records: f.Records}); err != nil {
		t.Fatal(err)
	}
}

// viewHeader is the counters a reader's view answers STATUS with.
func viewHeader(t *testing.T, h mailbox.UserIndex, id uint64) [5]uint32 {
	t.Helper()
	f, err := h.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	return [5]uint32{f.Messages, f.Unseen, f.NextUID, f.Recent, f.UIDValidity}
}

// writerRecords is the writer's own records as bytes, as it holds them after
// applying its changes through the replay.
func writerRecords(t *testing.T, h mailbox.UserIndex, id uint64) []byte {
	t.Helper()
	fs, err := h.(*userHandle).ui.state(id)
	if err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	// A writer that left its records on disk reads them back as a write does.
	if err := fs.reload(); err != nil {
		t.Fatal(err)
	}
	if fs.recs != nil {
		return fs.recs.Bytes()
	}
	layout, err := mailindex.ComputeRecordLayout(fs.file.Extensions)
	if err != nil {
		t.Fatal(err)
	}
	r, err := mailindex.ChunksFromRecords(fs.file.Records, layout)
	if err != nil {
		t.Fatal(err)
	}
	return r.Bytes()
}
