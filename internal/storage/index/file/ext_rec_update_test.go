package file

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// extIntro names extension name with the given generation and data width.
func extIntro(t *testing.T, fs *folderState, name string, reset uint32, width uint16) []byte {
	t.Helper()
	for i, e := range fs.file.Extensions {
		if e.Name == name {
			return encLogRec(mailindex.TxTypeExtIntro, 0, mailindex.EncodeTxExtIntroPayload(mailindex.TxExtIntro{
				ExtID: uint32(i), ResetID: reset, HdrSize: e.HdrSize, RecordSize: width, RecordAlign: e.RecordAlign,
			}))
		}
	}
	t.Fatalf("no extension %q", name)
	return nil
}

func extUpdate(width int, upds ...mailindex.TxExtRecUpdate) []byte {
	return encLogRec(mailindex.TxTypeExtRecUpdate, 0, mailindex.EncodeTxExtRecUpdatePayload(width, upds))
}

// The per-record extension update follows the reference's rules, the same on
// chunks as on decoded records (INDEX-MEMORY.md §4; #2628).
func TestARecordUpdateFollowsTheReferencesRules(t *testing.T) {
	cases := []struct {
		name    string
		recs    func(t *testing.T, fs *folderState, reset uint32) [][]byte
		wantErr bool
		// want is uid 5's cache bytes after the records.
		want []byte
	}{
		{name: "applied after its intro", recs: func(t *testing.T, fs *folderState, reset uint32) [][]byte {
			return [][]byte{extIntro(t, fs, extNameCache, reset, 4), extUpdate(4, mailindex.TxExtRecUpdate{UID: 5, Data: encodeCacheRec(512)})}
		}, want: encodeCacheRec(512)},
		{name: "no intro before it", recs: func(t *testing.T, fs *folderState, reset uint32) [][]byte {
			return [][]byte{extUpdate(4, mailindex.TxExtRecUpdate{UID: 5, Data: encodeCacheRec(512)})}
		}, wantErr: true},
		{name: "an older generation is ignored", recs: func(t *testing.T, fs *folderState, reset uint32) [][]byte {
			return [][]byte{extIntro(t, fs, extNameCache, reset-1, 4), extUpdate(4, mailindex.TxExtRecUpdate{UID: 5, Data: encodeCacheRec(512)})}
		}, want: encodeCacheRec(0)},
		{name: "uid 0 is damage", recs: func(t *testing.T, fs *folderState, reset uint32) [][]byte {
			return [][]byte{extIntro(t, fs, extNameCache, reset, 4), extUpdate(4, mailindex.TxExtRecUpdate{UID: 0, Data: encodeCacheRec(512)})}
		}, wantErr: true},
		{name: "a uid gone is skipped", recs: func(t *testing.T, fs *folderState, reset uint32) [][]byte {
			return [][]byte{extIntro(t, fs, extNameCache, reset, 4), extUpdate(4,
				mailindex.TxExtRecUpdate{UID: 999999, Data: encodeCacheRec(7)}, mailindex.TxExtRecUpdate{UID: 5, Data: encodeCacheRec(512)})}
		}, want: encodeCacheRec(512)},
		{name: "shorter data is padded with zeros", recs: func(t *testing.T, fs *folderState, reset uint32) [][]byte {
			return [][]byte{
				extIntro(t, fs, extNameCache, reset, 4), extUpdate(4, mailindex.TxExtRecUpdate{UID: 5, Data: encodeCacheRec(0xffffffff)}),
				// Two bytes of data and two of padding the writer left dirty: the
				// extension's other two bytes are zeros, not the padding.
				extIntro(t, fs, extNameCache, reset, 2), encLogRec(mailindex.TxTypeExtRecUpdate, 0, []byte{5, 0, 0, 0, 0x34, 0x12, 0xff, 0xff}),
			}
		}, want: encodeCacheRec(0x1234)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var forms [2][]byte
			for i, decoded := range []bool{false, true} {
				home := movedFolder(t, 2*mailindex.ChunkRecords)
				// Both cache extensions in the base, so the writer's layout
				// stays and it holds chunks after a reopen.
				o, of := acceptOpen(t, home)
				if err := o.(*userHandle).ui.SetCacheOffsets(of.ID, map[uint32]mailbox.CacheStamp{1: {Offset: 64, CRC: 1}}); err != nil {
					t.Fatal(err)
				}
				if err := o.OptimizeIndex(of.ID); err != nil {
					t.Fatal(err)
				}
				o.Close() //nolint:errcheck
				writerDecoded = decoded
				h, f := acceptOpen(t, home)
				fs, err := h.(*userHandle).ui.state(f.ID)
				if err != nil {
					t.Fatal(err)
				}
				fs.mu.Lock()
				if err := fs.reload(); err != nil {
					t.Fatal(err)
				}
				if err := fs.loadWriterRecords(); err != nil {
					t.Fatal(err)
				}
				if (fs.recs == nil) != decoded {
					t.Fatalf("decoded %v, yet the writer holds chunks: %v", decoded, fs.recs != nil)
				}
				reset := findExt(fs.file.Extensions, extNameCache).ResetID
				aerr := fs.applyRecords(c.recs(t, fs, reset))
				seq, _ := fs.recSeqOfUID(5)
				got, _ := fs.recExt(seq, extNameCache)
				forms[i] = append([]byte(nil), got...)
				fs.mu.Unlock()
				h.Close() //nolint:errcheck
				writerDecoded = false
				if c.wantErr {
					if aerr == nil {
						t.Errorf("decoded %v: the records applied, want an error", decoded)
					}
					continue
				}
				if aerr != nil {
					t.Fatalf("decoded %v: %v", decoded, aerr)
				}
				if !bytes.Equal(got, c.want) {
					t.Errorf("decoded %v: uid 5's cache is %x, want %x", decoded, got, c.want)
				}
			}
			if !c.wantErr && !bytes.Equal(forms[0], forms[1]) {
				t.Errorf("chunks give %x, decoded records %x", forms[0], forms[1])
			}
		})
	}
}

// A cache offset journalled before the cache was purged is not applied after
// it: it points into a file that is gone.
func TestACacheOffsetFromBeforeAPurgeIsNotApplied(t *testing.T) {
	home := movedFolder(t, 300)
	h, f := acceptOpen(t, home)
	defer h.Close() //nolint:errcheck
	u := h.(*userHandle).ui
	if err := u.SetCacheOffsets(f.ID, map[uint32]mailbox.CacheStamp{1: {Offset: 64, CRC: 1}}); err != nil {
		t.Fatal(err)
	}
	fs, err := u.state(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	before := findExt(fs.file.Extensions, extNameCache).ResetID
	stale := [][]byte{extIntro(t, fs, extNameCache, before, 4), extUpdate(4, mailindex.TxExtRecUpdate{UID: 9, Data: encodeCacheRec(4096)})}
	fs.mu.Unlock()
	// A cache file that does not open is purged into a new generation.
	if err := os.WriteFile(filepath.Join(fs.indexDir, mailindex.CacheFileName), []byte("not a cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := u.PurgeCache(f.ID); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	if findExt(fs.file.Extensions, extNameCache).ResetID == before {
		fs.mu.Unlock()
		t.Fatal("the purge kept the cache's generation")
	}
	// A writer that built its records before the purge appends them after it.
	if err := fs.appendMutLog(stale...); err != nil {
		t.Fatal(err)
	}
	fs.mu.Unlock()
	fresh, ff := acceptOpen(t, home)
	defer fresh.Close() //nolint:errcheck
	msgs, err := fresh.GetMessages(ff.ID, mailbox.SeqSet{{From: 9, To: 9}})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("read uid 9: %d, %v", len(msgs), err)
	}
	if msgs[0].CacheOffset != 0 {
		t.Errorf("uid 9's cache offset is %d after the purge, want none", msgs[0].CacheOffset)
	}
}
