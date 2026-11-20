package file

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// The bases each operation writes, fixed as bytes while the decoded path still
// stands beside the chunks: once it is gone there is nothing left to compare
// with (reviewer, 2026-11-20). YARILO_UPDATE_GOLDEN=1 writes them anew.
var updateGolden = os.Getenv("YARILO_UPDATE_GOLDEN") != ""

const goldenClock = 1700000000

type goldenOp struct {
	name string
	run  func(t *testing.T, h mailbox.UserIndex, u *userIndex, id uint64, home string)
	// file names what is compared: the folder's base, or the GUID store's.
	guidStore bool
}

var goldenOps = []goldenOp{
	{name: "fold"},
	{name: "align-uidvalidity", run: func(t *testing.T, _ mailbox.UserIndex, u *userIndex, id uint64, _ string) {
		if _, _, err := u.AlignUIDSpace(id, 99, 0); err != nil {
			t.Fatal(err)
		}
	}},
	{name: "adopt-uid-space", run: func(t *testing.T, _ mailbox.UserIndex, u *userIndex, id uint64, _ string) {
		// Adopted only by an empty folder, as a store hands over its own.
		if _, err := u.ResetFolder(id, nil); err != nil {
			t.Fatal(err)
		}
		if err := u.AdoptUIDSpace(id, 99, 900); err != nil {
			t.Fatal(err)
		}
	}},
	{name: "reset-folder", run: func(t *testing.T, _ mailbox.UserIndex, u *userIndex, id uint64, _ string) {
		var keep []*mailbox.MessageMeta
		for uid := uint32(1); uid <= 40; uid += 3 {
			keep = append(keep, goldenMeta(uid))
		}
		if _, err := u.ResetFolder(id, keep); err != nil {
			t.Fatal(err)
		}
	}},
	{name: "repair-tails", run: func(t *testing.T, _ mailbox.UserIndex, u *userIndex, id uint64, _ string) {
		if _, err := u.RepairRecordTails(id, map[uint32]mailbox.RecordTail{3: {MapUID: 77, SaveDate: 1700000100, VSize: 333, GUID: [16]byte{0xee, 3}}}); err != nil {
			t.Fatal(err)
		}
	}},
	{name: "purge-cache", run: func(t *testing.T, _ mailbox.UserIndex, u *userIndex, id uint64, _ string) {
		if err := u.SetCacheOffsets(id, map[uint32]mailbox.CacheStamp{2: {Offset: 64, CRC: 5}, 9: {Offset: 128, CRC: 6}}); err != nil {
			t.Fatal(err)
		}
		fs, err := u.state(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fs.indexDir, mailindex.CacheFileName), []byte("not a cache"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := u.PurgeCache(id); err != nil {
			t.Fatal(err)
		}
	}},
	{name: "new-extension", run: func(t *testing.T, h mailbox.UserIndex, _ *userIndex, id uint64, _ string) {
		m := goldenMeta(goldenRecords + 1)
		m.MapUID, m.SaveDate = 4242, 1700000200
		if err := h.AppendMessage(id, m); err != nil {
			t.Fatal(err)
		}
	}},
	{name: "guid-store-rebuild", guidStore: true, run: func(t *testing.T, _ mailbox.UserIndex, u *userIndex, id uint64, _ string) {
		if _, err := u.RebuildGUIDStore([]uint64{id}); err != nil {
			t.Fatal(err)
		}
	}},
}

const goldenRecords = 2*mailindex.ChunkRecords + 5

func goldenMeta(uid uint32) *mailbox.MessageMeta {
	return &mailbox.MessageMeta{UID: uid, Size: 100 + uid, VSize: 120 + uid, GUID: [16]byte{byte(uid), byte(uid >> 8), 0x5a},
		InternalDate: time.Unix(1690000000+int64(uid), 0), Flags: [][]string{nil, {`\Seen`}, {`\Seen`, `\Flagged`}}[uid%3]}
}

// goldenBase builds the folder, runs op, folds, and returns the bytes of the
// file op is about, on chunks or on decoded records.
func goldenBase(t *testing.T, op goldenOp, decoded bool) []byte {
	t.Helper()
	clock := nowUnix
	nowUnix = func() int64 { return goldenClock }
	defer func() { nowUnix = clock }()
	home := t.TempDir()
	open := func() (mailbox.UserIndex, *mailbox.Folder) {
		h := New(WithGUIDSource(func() [16]byte { return [16]byte{0x60, 0x1d} })).OpenUser(&mailbox.UserInfo{Username: testUser, Home: home})
		f, err := h.OpenFolder("INBOX", 7)
		if err != nil {
			t.Fatal(err)
		}
		return h, f
	}
	h, f := open()
	for uid := uint32(1); uid <= goldenRecords; uid++ {
		if err := h.AppendMessage(f.ID, goldenMeta(uid)); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.OptimizeIndex(f.ID); err != nil {
		t.Fatal(err)
	}
	h.Close() //nolint:errcheck
	if op.name == "new-extension" {
		withoutExtension(t, home, extNameMdbox)
	}
	baseImages.Delete(indexPathFor(home))
	writerDecoded, decodedImages = decoded, decoded
	defer func() { writerDecoded, decodedImages = false, false }()
	h, f = open()
	defer h.Close() //nolint:errcheck
	u := h.(*userHandle).ui
	if op.run != nil {
		op.run(t, h, u, f.ID, home)
	}
	if err := h.OptimizeIndex(f.ID); err != nil {
		t.Fatal(err)
	}
	path := indexPathFor(home)
	if op.guidStore {
		path = u.GUIDStorePath()
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestEveryOperationWritesTheGoldenBase(t *testing.T) {
	for _, op := range goldenOps {
		t.Run(op.name, func(t *testing.T) {
			fromChunks := goldenBase(t, op, false)
			again := goldenBase(t, op, false)
			if !bytes.Equal(fromChunks, again) {
				t.Fatal("two runs wrote different bytes: the base is not reproducible")
			}
			golden := filepath.Join("testdata", "golden", op.name+".index")
			if updateGolden {
				fromDecoded := goldenBase(t, op, true)
				if !bytes.Equal(fromChunks, fromDecoded) {
					t.Fatalf("chunks wrote %d bytes, decoded records %d, and they differ", len(fromChunks), len(fromDecoded))
				}
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, fromDecoded, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(fromChunks, want) {
				t.Errorf("the base from chunks (%d bytes) is not the golden one (%d)", len(fromChunks), len(want))
			}
		})
	}
}
