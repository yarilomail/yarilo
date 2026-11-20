package file

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

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
// lookup built for the base answers them from the journal, until the journal
// folds into a new base, which gets one lookup.
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
	if got := ask(9); len(got) != 1 || *builds != 2 {
		t.Errorf("after the fold: %v, %d builds, want one more", got, *builds)
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
