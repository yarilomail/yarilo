package mdboxmap

import (
	"os"
	"path/filepath"
	"testing"
)

// Every write the map makes fails when the disk refuses it, and the map still
// reads what it held, in this process and the next.
func TestMapWritesTheDiskRefusesLeaveItReadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through file modes")
	}
	for name, write := range map[string]func(m *Map, uid uint32) error{
		"append": func(m *Map, _ uint32) error { _, err := m.AppendRecord(1, 100, 10, [16]byte{9}); return err },
		"append records": func(m *Map, _ uint32) error {
			_, err := m.AppendRecords([]RecordLayout{{FileID: 1, Offset: 100, Size: 10}})
			return err
		},
		"append batch": func(m *Map, _ uint32) error {
			b := m.AppendBatch()
			b.Next(10)
			b.SetLastGUID([16]byte{7})
			_, err := b.Finish()
			return err
		},
		"refcount": func(m *Map, uid uint32) error { return m.UpdateRefcounts([]uint32{uid}, 1) },
		"set refcounts": func(m *Map, uid uint32) error {
			_, err := m.SetRefcountsFromReferences(map[uint32]int{uid: 3})
			return err
		},
		"move": func(m *Map, uid uint32) error {
			return m.AppendMove([]MovedRecord{{UID: uid, FileID: 2, Offset: 0, Size: 10}}, nil)
		},
		"expunge":       func(m *Map, _ uint32) error { _, err := m.ExpungeVanished(map[uint32]bool{}); return err },
		"compact":       func(m *Map, _ uint32) error { return m.Compact() },
		"file id":       func(m *Map, _ uint32) error { _, err := m.AllocFileID(); return err },
		"rebuild count": func(m *Map, _ uint32) error { return m.BumpRebuildCount() },
	} {
		t.Run(name, func(t *testing.T) {
			m, dir := openTestMap(t)
			uid := seedRecord(t, m, 1)
			lockDown(t, dir)
			if err := write(m, uid); err == nil {
				t.Error("the write succeeded on a disk that refuses writes")
			}
			for _, view := range []*Map{m, reopen(t, dir)} {
				if e, ok, err := view.LookupReloaded(uid); err != nil || !ok || e.FileID != 1 {
					t.Errorf("uid %d after the refused write: %+v ok=%v err=%v", uid, e, ok, err)
				}
			}
		})
	}
}

// lockDown makes the map's directory and files read-only until the test ends.
func lockDown(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if err := os.Chmod(p, 0o400); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}
