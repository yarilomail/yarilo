package file

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// writerFolder builds a folder of the given make-up by a sequence of writes,
// the last of them by a writer holding chunks, and folds it.
func writerFolder(t *testing.T, kind string) string {
	t.Helper()
	build := func() string {
		home := t.TempDir()
		h := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home})
		defer h.Close() //nolint:errcheck
		f, err := h.OpenFolder("INBOX", 7)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		if kind != "empty" {
			n = 2*mailindex.ChunkRecords + 9
		}
		for i := 1; i <= n; i++ {
			m := &mailbox.MessageMeta{UID: uint32(i), Size: 100 + uint32(i), VSize: 110, GUID: [16]byte{byte(i), byte(i >> 8)},
				InternalDate: time.Unix(1700000000+int64(i), 0)}
			if kind == "mdbox" {
				m.MapUID, m.SaveDate = uint32(1000+i), 1788000000
			}
			if err := h.AppendMessage(f.ID, m); err != nil {
				t.Fatal(err)
			}
		}
		if err := h.OptimizeIndex(f.ID); err != nil {
			t.Fatal(err)
		}
		// A reopen loads the base the way every later writer does.
		h.Close() //nolint:errcheck
		h = New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home})
		if f, err = h.OpenFolder("INBOX", 0); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			if err := h.UpdateFlags(f.ID, 1, []string{`\Answered`}, nil); err != nil {
				t.Fatal(err)
			}
			fs, err := h.(*userHandle).ui.state(f.ID)
			if err != nil {
				t.Fatal(err)
			}
			if fs.recs == nil {
				t.Fatal("the writer holds no chunks")
			}
		}
		switch kind {
		case "keywords":
			for i := 1; i <= n; i += 7 {
				if err := h.UpdateFlags(f.ID, uint32(i), []string{`\Seen`}, []string{"$K" + strconv.Itoa(i%3)}); err != nil {
					t.Fatal(err)
				}
			}
		case "expunged":
			tx, err := h.Begin(f.ID)
			if err != nil {
				t.Fatal(err)
			}
			for i := 3; i <= n; i += 11 {
				tx.Expunge(uint32(i))
			}
			if _, err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
		if err := h.OptimizeIndex(f.ID); err != nil {
			t.Fatal(err)
		}
		return home
	}
	return build()
}

// recordsSection is a base's records as they lie on disk, and its header
// counters and digest.
func recordsSection(t *testing.T, home string) ([]byte, [4]uint32, uint64) {
	t.Helper()
	f, err := mailindex.Open(indexPathFor(home))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(indexPathFor(home))
	if err != nil {
		t.Fatal(err)
	}
	h := f.Header
	return raw[h.HeaderSize:], [4]uint32{h.NextUID, h.MessagesCount, h.SeenMessagesCount, h.DeletedMessagesCount}, readLineage(f).RecordsDigest
}

// encodedByMailindex is the records section mailindex's own encoder writes
// for the records of the base at home, decoded from the same file: the
// encoder the decoded writer wrote with.
func encodedByMailindex(t *testing.T, home string) []byte {
	t.Helper()
	f, err := mailindex.Open(indexPathFor(home))
	if err != nil {
		t.Fatal(err)
	}
	// Field by field into fresh records, so a byte the writer left outside
	// every field is not carried over.
	var out []byte
	for i := 0; i < f.Records.Len(); i++ {
		rec := f.Layout.NewRecord(f.Records.UID(i), f.Records.Flags(i))
		for _, ext := range f.Layout.Extensions {
			if ext.RecordSize == 0 {
				continue
			}
			if err := f.Layout.SetExt(rec, ext.Name, f.Records.ExtNamed(i, ext.Name)); err != nil {
				t.Fatal(err)
			}
		}
		out = append(out, rec...)
	}
	return out
}

// A base the writer writes from chunks holds the records mailindex's encoder
// writes for them, byte for byte, with counters that match them and the
// digest develop computed, for folders of every make-up. The decoded writer it
// was compared with is gone; the golden bases hold its bytes.
func TestTheWriterWritesTheSameBaseFromChunks(t *testing.T) {
	for _, kind := range []string{"plain", "keywords", "expunged", "mdbox", "empty"} {
		t.Run(kind, func(t *testing.T) {
			home := writerFolder(t, kind)
			raw, counters, digest := recordsSection(t, home)
			if want := encodedByMailindex(t, home); !bytes.Equal(raw, want) {
				t.Errorf("the records on disk (%d bytes) are not mailindex's encoding of them (%d)", len(raw), len(want))
			}
			f, err := mailindex.Open(indexPathFor(home))
			if err != nil {
				t.Fatal(err)
			}
			var seen, deleted uint32
			for i := 0; i < f.Records.Len(); i++ {
				if f.Records.Flags(i)&mailindex.FlagSeen != 0 {
					seen++
				}
				if f.Records.Flags(i)&mailindex.FlagDeleted != 0 {
					deleted++
				}
			}
			if counters[1] != uint32(f.Records.Len()) || counters[2] != seen || counters[3] != deleted {
				t.Errorf("header counters %v for %d records, %d seen, %d deleted", counters, f.Records.Len(), seen, deleted)
			}
			if want := developDigest(f); digest != want {
				t.Errorf("records digest %x, develop's %x", digest, want)
			}
		})
	}
}

// An old folder without the GUID extension gains it through SetGUIDs: no
// record lost, every GUID in place, and the base on disk mailindex's encoding
// of its records.
func TestAnOldFolderGainsGUIDsWithoutLosingARecord(t *testing.T) {
	const n = 2*mailindex.ChunkRecords + 3
	home := movedFolder(t, n)
	withoutExtension(t, home, extNameGUID)
	h, f := acceptOpen(t, home)
	defer h.Close() //nolint:errcheck
	guids := map[uint32][16]byte{}
	for uid := uint32(1); uid <= n; uid++ {
		guids[uid] = [16]byte{0xa0, byte(uid), byte(uid >> 8)}
	}
	if err := h.(*userHandle).ui.SetGUIDs(f.ID, guids); err != nil {
		t.Fatal(err)
	}
	msgs, err := h.GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != n {
		t.Fatalf("%d messages after the stamp, want %d", len(msgs), n)
	}
	for _, m := range msgs {
		if m.GUID != guids[m.UID] {
			t.Fatalf("uid %d has guid %x, want %x", m.UID, m.GUID, guids[m.UID])
		}
	}
	if err := h.OptimizeIndex(f.ID); err != nil {
		t.Fatal(err)
	}
	raw, _, _ := recordsSection(t, home)
	if want := encodedByMailindex(t, home); !bytes.Equal(raw, want) {
		t.Errorf("the stamped base's records (%d bytes) are not mailindex's encoding of them (%d)", len(raw), len(want))
	}
}

// A transaction the replay refuses part way is rolled back on chunks byte for
// byte, with and without appends and expunges in it.
func TestARefusedTransactionLeavesTheChunksAsTheyWere(t *testing.T) {
	for _, structural := range []bool{false, true} {
		t.Run(map[bool]string{false: "flags", true: "appends and expunges"}[structural], func(t *testing.T) {
			home := movedFolder(t, 2*mailindex.ChunkRecords)
			h, f := acceptOpen(t, home)
			defer h.Close() //nolint:errcheck
			before := writerRecords(t, h, f.ID)
			tx, err := h.Begin(f.ID)
			if err != nil {
				t.Fatal(err)
			}
			tx.UpdateFlags(3, mailbox.FlagsUpdate{Flags: []string{`\Flagged`}})
			tx.UpdateFlags(mailindex.ChunkRecords+5, mailbox.FlagsUpdate{Flags: []string{`\Answered`}})
			if structural {
				tx.Expunge(9)
				tx.Append(&mailbox.MessageMeta{Size: 10})
			}
			refused := errors.New("refused")
			applied := 0
			replaySeam = func() error {
				if applied++; applied == 4 {
					return refused
				}
				return nil
			}
			_, cerr := tx.Commit()
			replaySeam = nil
			if !errors.Is(cerr, refused) {
				t.Fatalf("the transaction went through: %v", cerr)
			}
			if after := writerRecords(t, h, f.ID); !bytes.Equal(after, before) {
				t.Errorf("the records changed under a refused transaction: %d bytes, were %d", len(after), len(before))
			}
		})
	}
}

// A change of a chunk the writer already owns copies nothing (row 5's second
// bound for the writer).
func TestAWriterChangesItsOwnChunkInPlace(t *testing.T) {
	home := movedFolder(t, 2*mailindex.ChunkRecords)
	h, f := acceptOpen(t, home)
	defer h.Close() //nolint:errcheck
	if err := h.UpdateFlags(f.ID, 5, []string{`\Seen`}, nil); err != nil {
		t.Fatal(err)
	}
	fs, err := h.(*userHandle).ui.state(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	copies := fs.recs.ChunkCopies()
	fs.mu.Unlock()
	for uid := uint32(6); uid < 20; uid++ {
		if err := h.UpdateFlags(f.ID, uid, []string{`\Flagged`}, nil); err != nil {
			t.Fatal(err)
		}
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.recs == nil {
		t.Fatal("the writer holds no chunks")
	}
	if got := fs.recs.ChunkCopies() - copies; got != 0 {
		t.Errorf("14 changes of a chunk the writer owns copied %d chunks", got)
	}
}

// developDigest is the records digest as develop computed it, kept verbatim:
// bases on disk carry it, and a reload adopts a base only on an equal one.
func developDigest(f *mailindex.File) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	write := func(b []byte) {
		for _, c := range b {
			h ^= uint64(c)
			h *= prime64
		}
	}
	var scratch [8]byte
	var zero [1]byte
	for i := 0; i < f.Records.Len(); i++ {
		binary.LittleEndian.PutUint32(scratch[0:4], f.Records.UID(i))
		binary.LittleEndian.PutUint32(scratch[4:8], uint32(f.Records.Flags(i)))
		write(scratch[:])
		// Hashed as on disk: an unset extension is its width in zeros, and one
		// holding no record bytes is not part of a record at all.
		for _, ext := range f.Extensions {
			if ext.RecordSize == 0 {
				continue
			}
			write([]byte(ext.Name))
			b := f.Records.ExtNamed(i, ext.Name)
			if len(b) > int(ext.RecordSize) {
				b = b[:ext.RecordSize]
			}
			write(b)
			for range int(ext.RecordSize) - len(b) {
				write(zero[:])
			}
		}
	}
	return h
}

// The digest from chunks is the one develop wrote into every base, so a base
// written before this change is still adopted.
func TestTheRecordsDigestIsTheOneOnDisk(t *testing.T) {
	chunked := writerFolder(t, "keywords")
	f, err := mailindex.Open(indexPathFor(chunked))
	if err != nil {
		t.Fatal(err)
	}
	_, recs, err := mailindex.OpenLazy(indexPathFor(chunked))
	if err != nil {
		t.Fatal(err)
	}
	if err := recs.LoadAll(); err != nil {
		t.Fatal(err)
	}
	fromChunks := (&folderState{file: f, recs: recs}).digestRecords()
	if want := developDigest(f); fromChunks != want || want == 0 {
		t.Errorf("digest from chunks %x, develop's %x", fromChunks, want)
	}
}

// A handle that opened the folder and read no record takes the hold after
// another process rewrote the base over an empty journal: it reads the new
// base, it does not prove it against chunks it never loaded (#2630 review).
func TestAWriterWithChunksUnloadedTakesARewrittenBase(t *testing.T) {
	home := movedFolder(t, 3*mailindex.ChunkRecords)
	// The journal paired with the base, as after any fold: the next rewrite
	// then names this base as the one it continues.
	o, of := acceptOpen(t, home)
	if err := o.OptimizeIndex(of.ID); err != nil {
		t.Fatal(err)
	}
	o.Close() //nolint:errcheck
	a, af := acceptOpen(t, home)
	defer a.Close() //nolint:errcheck
	// Another process: its own image of the base, not the one a forked.
	baseImages.Delete(indexPathFor(home))
	b, bf := acceptOpen(t, home)
	// A rewrite that journals nothing first: the new base folds exactly the
	// empty journal the reader opened on.
	if err := b.(*userHandle).RecomputeVSize(bf.ID); err != nil {
		t.Fatal(err)
	}
	b.Close() //nolint:errcheck
	if err := a.UpdateFlags(af.ID, 8, []string{`\Flagged`}, nil); err != nil {
		t.Fatal(err)
	}
	msgs, err := a.GetMessages(af.ID, mailbox.SeqSet{{From: 7, To: 8}})
	if err != nil || len(msgs) != 2 {
		t.Fatalf("read back %d: %v", len(msgs), err)
	}
	if len(msgs[1].Flags) == 0 || msgs[1].Flags[0] != `\Flagged` {
		t.Errorf("uid 8 flags %v after the write", msgs[1].Flags)
	}
}

// An append below the last uid, as a caller naming its own uids may make,
// takes its place in the uid order the chunks' searches stand on.
func TestAnAppendBelowTheLastUIDTakesItsPlace(t *testing.T) {
	home := movedFolder(t, mailindex.ChunkRecords)
	h, f := acceptOpen(t, home)
	defer h.Close() //nolint:errcheck
	fs, err := h.(*userHandle).ui.state(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := h.Begin(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx.Expunge(40)
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []uint32{uint32(mailindex.ChunkRecords) + 5, 40} {
		if err := h.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Size: 10, Flags: []string{`\Flagged`}}); err != nil {
			t.Fatal(err)
		}
	}
	fs.mu.Lock()
	held := fs.recs != nil
	fs.mu.Unlock()
	if !held {
		t.Fatal("the writer holds no chunks")
	}
	msgs, err := h.GetMessages(f.ID, mailbox.SeqSet{{From: 39, To: 41}})
	if err != nil || len(msgs) != 3 || msgs[1].UID != 40 || len(msgs[1].Flags) == 0 {
		t.Fatalf("uids 39 to 41 read back as %+v: %v", msgs, err)
	}
	if err := h.UpdateFlags(f.ID, 40, []string{`\Seen`}, nil); err != nil {
		t.Fatal(err)
	}
	if got := metaOfIdx(t, h, f.ID, 40); got == nil || got.Flags[0] != `\Seen` {
		t.Errorf("uid 40 after a change: %+v", got)
	}
}

func metaOfIdx(t *testing.T, h mailbox.UserIndex, id uint64, uid uint32) *mailbox.MessageMeta {
	t.Helper()
	msgs, err := h.GetMessages(id, mailbox.SeqSet{{From: uid, To: uid}})
	if err != nil || len(msgs) != 1 {
		return nil
	}
	return msgs[0]
}
