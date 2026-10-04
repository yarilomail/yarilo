//go:build flatcurve

package flatcurve

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/0kaba0hub/go-xapian"
	"github.com/yarilomail/yarilo/pkg/fts"
)

// A copy belongs to its message's document wherever that sits: adding reached
// the current shard only, while the retraction already walked them all (#1986).
func TestACopyJoinsTheMessageInASealedShard(t *testing.T) {
	ui, _ := testEngine(t, Options{RotateCount: 1})
	other := fts.MailboxRef{GUID: "g2", Name: "Archive", UIDValidity: 1}

	indexDoc(t, ui, 1, []string{"plan"}, []string{"alpha"})
	// Rotation seals the shard the message went into.
	indexDoc(t, ui, 2, []string{"other"}, []string{"bravo"})
	indexCopy(t, ui, other, 7, testGUID(1), []string{"plan"}, []string{"alpha"})

	u := ui.(*userIndex)
	paths, err := shardPaths(u.state().dir)
	if err != nil {
		t.Fatal(err)
	}
	docs := 0
	for _, p := range paths {
		w, oerr := xapian.OpenWDB(p)
		if oerr != nil {
			t.Fatal(oerr)
		}
		ids, derr := w.DocIDsByTerm(guidTerm(testGUID(1)))
		w.Close()
		if derr != nil {
			t.Fatal(derr)
		}
		docs += len(ids)
	}
	if docs != 1 {
		t.Errorf("the message has %d documents across %d shards, want one", docs, len(paths))
	}

	// And the copy is searchable in its own folder, not only in the original's.
	res, err := ui.Lookup([]string{other.GUID}, bodyQuery("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if got := uidsOf(append(res.DefiniteGUIDs, res.MaybeGUIDs...)); len(got) != 1 {
		t.Errorf("the copy's folder answers %v, want the message", got)
	}
}

// A delivery finds its message in no sealed shard, so it must not open one for
// writing; only a copy that lands on an older document pays for that.
func TestADeliveryOpensNoSealedShardForWriting(t *testing.T) {
	ui, _ := testEngine(t, Options{RotateCount: 1})
	other := fts.MailboxRef{GUID: "g2", Name: "Archive", UIDValidity: 1}

	indexDoc(t, ui, 1, nil, []string{"alpha"})
	indexDoc(t, ui, 2, nil, []string{"bravo"})

	before := testutil.ToFloat64(metricSealedWriteOpen)
	indexDoc(t, ui, 3, nil, []string{"charlie"})
	if now := testutil.ToFloat64(metricSealedWriteOpen); now != before {
		t.Errorf("a new message opened %v sealed shards for writing, want none", now-before)
	}

	// The copy is the one that pays, and for one shard only.
	indexCopy(t, ui, other, 9, testGUID(1), nil, []string{"alpha"})
	if now := testutil.ToFloat64(metricSealedWriteOpen); now != before+1 {
		t.Errorf("joining a copy opened %v sealed shards for writing, want one", now-before)
	}
}
