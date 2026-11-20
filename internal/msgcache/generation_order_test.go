package msgcache

import (
	"errors"
	"os"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// An open that finds the cache invalid takes a new generation before it drops
// the file. Stopped between the two, no offset the index holds points into the
// cache the next open makes (#2634).
func TestAnInvalidCacheIsDroppedOnlyAfterTheNewGeneration(t *testing.T) {
	idx, fid, m := newFolder(t)
	h := Open(idx, fid, Options{})
	if h == nil {
		t.Fatal("no cache handle")
	}
	h.StoreEnvelope(m, nil)
	h.Close()
	ic, ok := idx.(Index)
	if !ok {
		t.Skip("index has no cache surface")
	}
	if err := ic.SetCacheOffsets(fid, map[uint32]mailbox.CacheStamp{m.UID: {Offset: 64, CRC: 1}}); err != nil {
		t.Fatal(err)
	}
	path, err := ic.CachePath(fid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a cache"), 0o600); err != nil {
		t.Fatal(err)
	}

	afterGenerationBump = func() error { return errors.New("stopped") }
	if stopped := Open(idx, fid, Options{}); stopped != nil {
		stopped.Close()
	}
	afterGenerationBump = nil

	// Whatever the stop left, the next open makes a cache of the generation
	// the index names; no stamp may point into it.
	if _, serr := os.Stat(path); os.IsNotExist(serr) {
		indexID, resetID, _, ierr := ic.CachePairIdentity(fid)
		if ierr != nil {
			t.Fatal(ierr)
		}
		nf, cerr := mailindex.CreateCache(path, indexID, resetID)
		if cerr != nil {
			t.Fatal(cerr)
		}
		nf.Close()
	}
	msgs, err := idx.GetMessages(fid, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	indexID, resetID, _, _ := ic.CachePairIdentity(fid)
	for _, got := range msgs {
		if got.CacheOffset == 0 {
			continue
		}
		if cf, oerr := mailindex.OpenCache(path, indexID, resetID); oerr == nil {
			cf.Close()
			t.Errorf("uid %d keeps offset %d into a cache of the generation the index names", got.UID, got.CacheOffset)
		}
	}
}
