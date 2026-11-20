package file

import (
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/sortedfile"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// lookupBuilds counts the lookups built while a test runs.
func lookupBuilds(t *testing.T) *int {
	t.Helper()
	n := new(int)
	guidLookupBuildSeam = func() { *n++ }
	t.Cleanup(func() { guidLookupBuildSeam = nil })
	return n
}

func sortedCopies(rs []mailbox.GUIDRecord) []mailbox.GUIDRecord {
	out := slices.Clone(rs)
	slices.SortFunc(out, func(a, b mailbox.GUIDRecord) int {
		return strings.Compare(fmt.Sprint(a.GUID, a.FolderGUID, a.UID), fmt.Sprint(b.GUID, b.FolderGUID, b.UID))
	})
	return out
}

// An append and an expunge go to the store's journal and rebuild nothing: the
// lookup built for the base answers them from the journal, and the fold that
// writes the next base merges the lookup forward instead of building it.
func TestAnAppendDoesNotRebuildTheGUIDLookup(t *testing.T) {
	ui, home := guidUser(t)
	builds := lookupBuilds(t)
	first := mailbox.GUIDRecord{GUID: [16]byte{1}, FolderGUID: [16]byte{7}, UID: 1, InternalDate: 1700000000}
	if err := ui.AppendGUIDRecord(first); err != nil {
		t.Fatal(err)
	}
	id, err := ui.openGUIDStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := ui.OptimizeIndex(id); err != nil {
		t.Fatal(err)
	}
	ask := func(g byte) []mailbox.GUIDRecord {
		t.Helper()
		got, err := ui.GUIDCopies([][16]byte{{g}})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := ask(1); len(got) != 1 || *builds != 1 {
		t.Fatalf("on the base: %v, %d builds", got, *builds)
	}
	for i := 2; i <= 50; i++ {
		if err := ui.AppendGUIDRecord(mailbox.GUIDRecord{GUID: [16]byte{byte(i)}, FolderGUID: [16]byte{7}, UID: uint32(i)}); err != nil {
			t.Fatal(err)
		}
		if got := ask(byte(i)); len(got) != 1 || got[0].UID != uint32(i) {
			t.Fatalf("append %d: %v", i, got)
		}
	}
	if err := ui.applyGUIDBatch(guidBatch{gone: []guidCopy{{folderGUID: [16]byte{7}, uid: 1}}}); err != nil {
		t.Fatal(err)
	}
	if got := ask(1); len(got) != 0 {
		t.Errorf("an expunged copy still answers: %v", got)
	}
	// As another process reads it: a fresh handle, the store's image not
	// loaded, the expunge pending.
	DropBaseImages()
	other := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	defer other.Close() //nolint:errcheck
	if got, err := other.GUIDCopies([][16]byte{{1}}); err != nil || len(got) != 0 {
		t.Errorf("an expunged copy answers from a lazy view: %v %v", got, err)
	}
	if got, err := other.GUIDCopies([][16]byte{{50}}); err != nil || len(got) != 1 {
		t.Errorf("a journalled copy from a lazy view: %v %v", got, err)
	}
	if *builds != 1 {
		t.Errorf("50 appends and an expunge built the lookup %d times, want once", *builds)
	}
	if err := ui.OptimizeIndex(id); err != nil {
		t.Fatal(err)
	}
	// The fold merges the lookup forward: nothing is built, everything answers.
	if got := ask(9); len(got) != 1 || *builds != 1 {
		t.Errorf("after the fold: %v, %d builds, want still one", got, *builds)
	}
	if got := ask(1); len(got) != 0 {
		t.Errorf("after the fold the expunged copy answers: %v", got)
	}
}

// Through random appends, expunges and folds, with the store's records loaded
// or not, every GUID answers what a read of the whole store answers.
func TestTheGUIDLookupAnswersWhatTheStoreHolds(t *testing.T) {
	for seed := uint64(1); seed <= 4; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, seed+1))
			ui, home := guidUser(t)
			type live struct {
				folder [16]byte
				uid    uint32
			}
			var copies []live
			uid := uint32(0)
			for step := 0; step < 300; step++ {
				switch k := rng.IntN(10); {
				case k < 7 || len(copies) == 0:
					uid++
					c := live{folder: [16]byte{byte(rng.IntN(3) + 1)}, uid: uid}
					if err := ui.AppendGUIDRecord(mailbox.GUIDRecord{GUID: [16]byte{byte(rng.IntN(40) + 1)}, FolderGUID: c.folder, UID: c.uid}); err != nil {
						t.Fatal(err)
					}
					copies = append(copies, c)
				case k < 9:
					i := rng.IntN(len(copies))
					c := copies[i]
					copies = append(copies[:i], copies[i+1:]...)
					if err := ui.applyGUIDBatch(guidBatch{gone: []guidCopy{{folderGUID: c.folder, uid: c.uid}}}); err != nil {
						t.Fatal(err)
					}
				default:
					id, err := ui.openGUIDStore()
					if err != nil {
						t.Fatal(err)
					}
					if err := ui.OptimizeIndex(id); err != nil {
						t.Fatal(err)
					}
				}
				if step%25 != 24 {
					continue
				}
				all, err := ui.GUIDRecords()
				if err != nil {
					t.Fatal(err)
				}
				// Half the time as another process reads: a fresh handle and
				// nothing loaded, the journal pending.
				reader := ui
				if rng.IntN(2) == 0 {
					DropBaseImages()
					reader = New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
					defer reader.Close() //nolint:errcheck
				}
				for g := 1; g <= 40; g++ {
					var want []mailbox.GUIDRecord
					for _, r := range all {
						if r.GUID == [16]byte{byte(g)} {
							want = append(want, r)
						}
					}
					got, err := reader.GUIDCopies([][16]byte{{byte(g)}})
					if err != nil {
						t.Fatal(err)
					}
					if !slices.Equal(sortedCopies(got), sortedCopies(want)) {
						t.Fatalf("step %d guid %d: %v from the lookup, %v in the store", step, g, got, want)
					}
				}
			}
		})
	}
}

// A file under the lookup's name that does not read as one, as a crash
// before its rename leaves it, is rebuilt, not an error until the base moves.
func TestATornGUIDLookupIsRebuilt(t *testing.T) {
	cases := []struct {
		name string
		body func(good []byte) []byte
	}{
		{"garbage", func([]byte) []byte { return []byte("not a lookup at all") }},
		{"empty", func([]byte) []byte { return nil }},
		{"its header only", func(good []byte) []byte { return good[:min(64, len(good))] }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ui, home := guidUser(t)
			for i := 1; i <= 20; i++ {
				if err := ui.AppendGUIDRecord(mailbox.GUIDRecord{GUID: [16]byte{byte(i)}, FolderGUID: [16]byte{7}, UID: uint32(i)}); err != nil {
					t.Fatal(err)
				}
			}
			id, err := ui.openGUIDStore()
			if err != nil {
				t.Fatal(err)
			}
			if err := ui.OptimizeIndex(id); err != nil {
				t.Fatal(err)
			}
			if _, err := ui.GUIDCopies([][16]byte{{3}}); err != nil {
				t.Fatal(err)
			}
			path := ui.GUIDStorePath() + guidLookupSuffix
			good, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, tc.body(good), 0o600); err != nil {
				t.Fatal(err)
			}
			other := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
			defer other.Close() //nolint:errcheck
			got, err := other.GUIDCopies([][16]byte{{3}, {17}})
			if err != nil || len(got) != 2 {
				t.Fatalf("over a %s lookup: %v %v", tc.name, got, err)
			}
		})
	}
}

// foldAlloc is what the fold of a store of n copies allocates, with a
// journal of 200 appends and 20 expunges, with the lookup merged or not.
func foldAlloc(t *testing.T, n int, merge bool) uint64 {
	t.Helper()
	ui, _ := guidUser(t)
	id, err := ui.openGUIDStore()
	if err != nil {
		t.Fatal(err)
	}
	batch := make([]mailbox.GUIDRecord, 0, n)
	for i := 1; i <= n; i++ {
		batch = append(batch, mailbox.GUIDRecord{GUID: [16]byte{byte(i), byte(i >> 8), byte(i >> 16)}, FolderGUID: [16]byte{7}, UID: uint32(i)})
	}
	if err := ui.applyGUIDBatch(guidBatch{add: batch}); err != nil {
		t.Fatal(err)
	}
	if err := ui.OptimizeIndex(id); err != nil {
		t.Fatal(err)
	}
	if _, err := ui.GUIDCopies([][16]byte{{1}}); err != nil {
		t.Fatal(err)
	}
	for i := n + 1; i <= n+200; i++ {
		if err := ui.AppendGUIDRecord(mailbox.GUIDRecord{GUID: [16]byte{byte(i), byte(i >> 8), byte(i >> 16)}, FolderGUID: [16]byte{7}, UID: uint32(i)}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 20; i++ {
		if err := ui.applyGUIDBatch(guidBatch{gone: []guidCopy{{folderGUID: [16]byte{7}, uid: uint32(i * 3)}}}); err != nil {
			t.Fatal(err)
		}
	}
	guidLookupMergeSeam = func() bool { return !merge }
	defer func() { guidLookupMergeSeam = nil }()
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	if err := ui.OptimizeIndex(id); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&m1)
	if merge {
		if got, want := lookupTag(t, ui), storeTag(t, ui, id); got != want {
			t.Fatalf("after the fold the lookup names base %x, the store's is %x: no merge ran", got, want)
		}
	}
	return m1.TotalAlloc - m0.TotalAlloc
}

func lookupTag(t *testing.T, ui *userIndex) uint64 {
	t.Helper()
	r, err := sortedfile.Open(ui.GUIDStorePath() + guidLookupSuffix)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close() //nolint:errcheck
	return r.Generation
}

func storeTag(t *testing.T, ui *userIndex, id uint64) uint64 {
	t.Helper()
	var tag uint64
	if err := ui.withFolderHeaderUnlocked(id, func(fs *folderState) error {
		tag = guidBaseTag(fs.file.Header.IndexID, fs.lineage.Lineage)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return tag
}

// The merge a fold makes streams the old lookup: what it allocates does not
// grow with the store (2000 against 10000 copies, 32 B a copy), where a
// build read and sorted every copy.
func TestAFoldMergesTheGUIDLookupWithoutReadingTheStore(t *testing.T) {
	cost := func(n int) float64 { return float64(foldAlloc(t, n, true)) - float64(foldAlloc(t, n, false)) }
	small, large := cost(2000), cost(10000)
	t.Logf("the lookup's merge allocates %.0f B at 2000 copies, %.0f B at 10000", small, large)
	if grow := (large - small) / 8000; grow > 32 {
		t.Errorf("the lookup's merge allocates %.0f B at 2000 copies, %.0f B at 10000: %.1f B a copy", small, large, grow)
	}
}

// A read holding the lookup a fold replaced finishes on it: the old reader
// closes when its last read is done, not when the next lookup opens.
func TestAReplacedGUIDLookupStaysOpenForItsReads(t *testing.T) {
	ui, _ := guidUser(t)
	if err := ui.AppendGUIDRecord(mailbox.GUIDRecord{GUID: [16]byte{1}, FolderGUID: [16]byte{7}, UID: 1}); err != nil {
		t.Fatal(err)
	}
	id, err := ui.openGUIDStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := ui.OptimizeIndex(id); err != nil {
		t.Fatal(err)
	}
	old, release, err := ui.guidLookup(storeTag(t, ui, id))
	if err != nil {
		t.Fatal(err)
	}
	if err := ui.AppendGUIDRecord(mailbox.GUIDRecord{GUID: [16]byte{2}, FolderGUID: [16]byte{7}, UID: 2}); err != nil {
		t.Fatal(err)
	}
	if err := ui.OptimizeIndex(id); err != nil {
		t.Fatal(err)
	}
	if _, rel2, err := ui.guidLookup(storeTag(t, ui, id)); err != nil {
		t.Fatal(err)
	} else {
		rel2()
	}
	if _, ok, err := old.Get(guidLookupSection, []byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}); err != nil || !ok {
		t.Errorf("the replaced lookup's read: %v %v", ok, err)
	}
	release()
}
