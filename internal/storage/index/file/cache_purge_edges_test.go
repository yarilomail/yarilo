package file

import (
	"os"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// A purge where there is nothing to purge does nothing: no cache ever, or a
// cache file that is already gone.
func TestAPurgeWithNothingCachedDoesNothing(t *testing.T) {
	t.Run("never cached", func(t *testing.T) {
		ui, id := cachedFolder(t, 0, 3, nil)
		carried, reclaimed, err := ui.PurgeCache(id)
		if err != nil || carried != 0 || reclaimed != 0 {
			t.Errorf("purge: carried %d reclaimed %d err %v", carried, reclaimed, err)
		}
		if path, _ := ui.CachePath(id); path != "" {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("the purge made a cache file: %v", err)
			}
		}
	})
	t.Run("file gone", func(t *testing.T) {
		ui, id := cachedFolder(t, 0, 3, []uint32{1, 2})
		path, _ := ui.CachePath(id)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		carried, reclaimed, err := ui.PurgeCache(id)
		if err != nil || carried != 0 || reclaimed != 0 {
			t.Errorf("purge: carried %d reclaimed %d err %v", carried, reclaimed, err)
		}
	})
}

// A stamp pointing at nothing the cache holds is dropped by the purge, not
// carried into the new generation where it would name someone else's bytes.
func TestAPurgeDropsAStampTheCacheCannotBack(t *testing.T) {
	ui, id := cachedFolder(t, 0, 3, []uint32{1})
	if err := ui.SetCacheOffsets(id, map[uint32]mailbox.CacheStamp{2: {Offset: 1 << 20}}); err != nil {
		t.Fatal(err)
	}
	carried, _, err := ui.PurgeCache(id)
	if err != nil {
		t.Fatal(err)
	}
	if carried != 1 {
		t.Errorf("carried %d records, want uid 1's alone", carried)
	}
	msgs, err := ui.GetMessages(id, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.UID == 2 && m.CacheOffset != 0 {
			t.Errorf("uid 2 still stamped at %d after the purge", m.CacheOffset)
		}
		if m.UID == 1 && m.CacheOffset == 0 {
			t.Error("uid 1 lost its stamp")
		}
	}
}
