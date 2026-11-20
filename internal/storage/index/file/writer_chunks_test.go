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

// writerFolder builds the same folder twice, once with the writer on chunks
// and once on decoded records, by one sequence of writes; it returns the two
// homes.
func writerFolder(t *testing.T, kind string) (chunked, decoded string) {
	t.Helper()
	build := func(asDecoded bool) string {
		writerDecoded = asDecoded
		defer func() { writerDecoded = false }()
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
		// A fresh folder's writer holds what it created; a reopen loads the base
		// the way every later writer does, as chunks unless decoded is asked.
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
			if held := fs.recs != nil; held == asDecoded {
				t.Fatalf("decoded %v, yet the writer holds chunks: %v", asDecoded, held)
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
	return build(false), build(true)
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

// A base the writer writes from chunks is the base it writes from decoded
// records, byte for byte in its records, with the same counters and the same
// records digest, for folders of every make-up.
func TestTheWriterWritesTheSameBaseFromChunks(t *testing.T) {
	for _, kind := range []string{"plain", "keywords", "expunged", "mdbox", "empty"} {
		t.Run(kind, func(t *testing.T) {
			chunked, decoded := writerFolder(t, kind)
			cr, ch, cd := recordsSection(t, chunked)
			dr, dh, dd := recordsSection(t, decoded)
			if !bytes.Equal(cr, dr) {
				t.Errorf("the records differ: %d bytes from chunks, %d decoded", len(cr), len(dr))
			}
			if ch != dh {
				t.Errorf("header counters %v from chunks, %v decoded", ch, dh)
			}
			if cd != dd || cd == 0 && len(cr) > 0 {
				t.Errorf("records digest %x from chunks, %x decoded", cd, dd)
			}
		})
	}
}

// An old folder without the GUID extension gains it through SetGUIDs: no
// record lost, every GUID in place, and the base as the decoded path writes it.
func TestAnOldFolderGainsGUIDsWithoutLosingARecord(t *testing.T) {
	stamp := func(asDecoded bool) (string, []*mailbox.MessageMeta) {
		home := movedFolder(t, 2*mailindex.ChunkRecords+3)
		withoutExtension(t, home, extNameGUID)
		writerDecoded = asDecoded
		defer func() { writerDecoded = false }()
		h, f := acceptOpen(t, home)
		defer h.Close() //nolint:errcheck
		guids := map[uint32][16]byte{}
		for uid := uint32(1); uid <= uint32(2*mailindex.ChunkRecords+3); uid++ {
			guids[uid] = [16]byte{0xa0, byte(uid), byte(uid >> 8)}
		}
		if err := h.(*userHandle).ui.SetGUIDs(f.ID, guids); err != nil {
			t.Fatal(err)
		}
		msgs, err := h.GetMessages(f.ID, mailbox.SeqSet{})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			if m.GUID != guids[m.UID] {
				t.Fatalf("decoded %v: uid %d has guid %x, want %x", asDecoded, m.UID, m.GUID, guids[m.UID])
			}
		}
		return home, msgs
	}
	chunked, cm := stamp(false)
	decoded, dm := stamp(true)
	if len(cm) != 2*mailindex.ChunkRecords+3 || len(cm) != len(dm) {
		t.Fatalf("%d and %d messages after the stamp, want %d", len(cm), len(dm), 2*mailindex.ChunkRecords+3)
	}
	cr, _, _ := recordsSection(t, chunked)
	dr, _, _ := recordsSection(t, decoded)
	if !bytes.Equal(cr, dr) {
		t.Errorf("the stamped bases differ: %d and %d record bytes", len(cr), len(dr))
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
	for _, rec := range f.Records {
		binary.LittleEndian.PutUint32(scratch[0:4], rec.UID)
		binary.LittleEndian.PutUint32(scratch[4:8], uint32(rec.Flags))
		write(scratch[:])
		// Hashed as on disk: an unset extension is its width in zeros, and one
		// holding no record bytes is not part of a record at all.
		for _, ext := range f.Extensions {
			if ext.RecordSize == 0 {
				continue
			}
			write([]byte(ext.Name))
			b := rec.Ext[ext.Name]
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
	chunked, _ := writerFolder(t, "keywords")
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
