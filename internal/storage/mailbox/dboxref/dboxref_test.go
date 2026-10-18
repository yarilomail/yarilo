package dboxref

import (
	"io/fs"
	"path"
	"sort"
	"testing"
)

// accessors calls every fixture function. A file no entry reads fails below,
// so a new fixture cannot be embedded without one.
var accessors = []func(t *testing.T){
	func(t *testing.T) { MdboxFile(t) },
	func(t *testing.T) { SdboxShort(t) },
	func(t *testing.T) { SdboxLong(t) },
	func(t *testing.T) { IndexBase(t) },
	func(t *testing.T) { IndexLog(t) },
	func(t *testing.T) { IndexLogRotated(t) },
	func(t *testing.T) { IndexFreshLog(t) },
	func(t *testing.T) { IndexTailLog(t) },
	func(t *testing.T) { MapTailLog(t) },
	func(t *testing.T) { StoreTailFile(t) },
	func(t *testing.T) { Subscriptions(t) },
	func(t *testing.T) { MapBase(t) },
	func(t *testing.T) { MapBaseLog(t) },
	func(t *testing.T) { MapRotatedBase(t) },
	func(t *testing.T) { MapRotatedLog(t) },
	func(t *testing.T) { MapRotatedPrev(t) },
	func(t *testing.T) { MapLog(t) },
	func(t *testing.T) { StoreFile(t) },
	func(t *testing.T) { SdboxInboxLog(t) },
	func(t *testing.T) { SdboxCyrillicLog(t) },
	func(t *testing.T) {
		for uid := 1; uid <= 4; uid++ {
			SdboxInboxMessage(t, uid)
		}
	},
}

// Every function reads an embedded file, and every embedded file but the
// README is read by a function.
func TestEveryFixtureHasAFunctionAndEveryFunctionAFile(t *testing.T) {
	read := map[string]bool{}
	readSeen = func(name string) { read[name] = true }
	t.Cleanup(func() { readSeen = nil })
	for _, call := range accessors {
		call(t)
	}
	var orphans []string
	err := fs.WalkDir(files, "testdata", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Base(p) == "README.md" {
			return err
		}
		if !read[p] {
			orphans = append(orphans, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(orphans)
	if len(orphans) > 0 {
		t.Errorf("embedded and read by no function: %v", orphans)
	}
}
