package mdbox

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/mdbox/mdboxmap"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// A rebuild leaves every folder record reading its own bytes, whatever the map
// said before (#2236).
func TestRebuildPairsEntriesWithTheirBytes(t *testing.T) {
	const bodyA, bodyB = "Subject: a\r\n\r\nfirst message\r\n", "Subject: b\r\n\r\nthe second one, longer\r\n"
	ga, gb := [16]byte{0xa}, [16]byte{0xb}
	for _, tc := range []struct {
		name string
		// register files map entries and folder records over B's bytes, then A's.
		register    func(t *testing.T, m *mdboxmap.Map, idx mailbox.UserIndex, a, b raw)
		wantRecords int
		want        func(t *testing.T, m *mdboxmap.Map, stats mailbox.StorageRebuildStats)
	}{
		{"entry at a later message's offset is repointed", func(t *testing.T, m *mdboxmap.Map, idx mailbox.UserIndex, a, b raw) {
			// The stand's shape: the lower map_uid names the place of the higher.
			fileRecord(t, idx, appendEntry(t, m, b, a.size, ga), ga)
			fileRecord(t, idx, appendEntry(t, m, b, b.size, gb), gb)
		}, 2, func(t *testing.T, _ *mdboxmap.Map, stats mailbox.StorageRebuildStats) {
			if stats.Repointed != 1 {
				t.Errorf("repointed %d, want 1", stats.Repointed)
			}
		}},
		{"entry whose GUID is nowhere vanishes", func(t *testing.T, m *mdboxmap.Map, idx mailbox.UserIndex, a, b raw) {
			fileRecord(t, idx, appendEntry(t, m, b, b.size, [16]byte{9}), [16]byte{9})
			fileRecord(t, idx, appendEntry(t, m, a, a.size, ga), ga)
			fileRecord(t, idx, appendEntry(t, m, b, b.size, gb), gb)
		}, 2, func(t *testing.T, m *mdboxmap.Map, _ mailbox.StorageRebuildStats) {
			if n := len(mustRecords(t, m)); n != 2 {
				t.Errorf("map holds %d records, want the two with bytes", n)
			}
		}},
		{"folder record follows its GUID", func(t *testing.T, m *mdboxmap.Map, idx mailbox.UserIndex, a, b raw) {
			fileRecord(t, idx, appendEntry(t, m, a, a.size, ga), ga)
			fileRecord(t, idx, appendEntry(t, m, b, b.size, gb), gb)
			fileRecord(t, idx, 999999, ga)
		}, 3, func(t *testing.T, _ *mdboxmap.Map, stats mailbox.StorageRebuildStats) {
			if stats.Remapped != 1 {
				t.Errorf("remapped %d, want 1", stats.Remapped)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box, idx := newBoxAndIndex(t, filepath.Join(t.TempDir(), "home"))
			m, err := box.openMap()
			if err != nil {
				t.Fatal(err)
			}
			b := writeRaw(t, box, bodyB, gb)
			a := writeRaw(t, box, bodyA, ga)
			tc.register(t, m, idx, a, b)

			stats, err := box.RebuildStorage(mailboxbase.Open(box, idx), idx, false)
			if err != nil {
				t.Fatalf("rebuild: %v", err)
			}
			tc.want(t, m, stats)

			f, _ := idx.OpenFolder("INBOX", 0)
			msgs, err := idx.GetMessages(f.ID, allMessages)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != tc.wantRecords || f.Messages != uint32(tc.wantRecords) {
				t.Fatalf("INBOX holds %d records, header says %d; want %d", len(msgs), f.Messages, tc.wantRecords)
			}
			want := map[[16]byte]string{ga: bodyA, gb: bodyB}
			for _, mm := range msgs {
				rc, err := box.Fetch("INBOX", strconv.FormatUint(uint64(mm.MapUID), 10), false)
				if err != nil {
					t.Fatalf("fetch uid %d (map_uid %d): %v", mm.UID, mm.MapUID, err)
				}
				got, _ := io.ReadAll(rc)
				_ = rc.Close()
				if string(got) != want[mm.GUID] {
					t.Errorf("uid %d (GUID %x) reads %q", mm.UID, mm.GUID, got)
				}
			}
		})
	}
}

// raw is one record written straight into m.1, with no map entry.
type raw struct{ file, off, size uint32 }

func writeRaw(t *testing.T, box *userMailbox, body string, guid [16]byte) raw {
	t.Helper()
	f, err := os.OpenFile(box.mfilePath(1), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	off, err := appendRecordToFile(f, []byte(body), guid, "INBOX", time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	return raw{file: 1, off: off, size: uint32(st.Size()) - off}
}

func appendEntry(t *testing.T, m *mdboxmap.Map, at raw, size uint32, guid [16]byte) uint32 {
	t.Helper()
	uid, err := m.AppendRecord(at.file, at.off, size, guid)
	if err != nil {
		t.Fatal(err)
	}
	return uid
}

func mustRecords(t *testing.T, m *mdboxmap.Map) []mdboxmap.MapEntry {
	t.Helper()
	e, err := m.RecordsReloaded()
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// fileRecord files a record in INBOX naming mapUID with guid.
func fileRecord(t *testing.T, idx mailbox.UserIndex, mapUID uint32, guid [16]byte) {
	t.Helper()
	f, _ := idx.OpenFolder("INBOX", 0)
	uid, err := idx.AllocateUID(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, MapUID: mapUID, Size: 4, GUID: guid}); err != nil {
		t.Fatal(err)
	}
}
