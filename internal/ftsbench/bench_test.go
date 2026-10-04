//go:build flatcurve

package ftsbench

import (
	"testing"
)

// indexRatioCap bounds on-disk index size as a multiple of the corpus
// (https://doc.yarilomail.org/FTS §12); catches e.g. accidental substring indexing.
const indexRatioCap = 3.0

// TestAcceptance fails when indexed SEARCH is slower than a brute-force
// scan or the index grows past indexRatioCap (https://doc.yarilomail.org/FTS §12).
func TestAcceptance(t *testing.T) {
	rep, err := Run(Config{
		Root:       t.TempDir(),
		Corpus:     1500,
		HitEvery:   100,
		Iterations: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("\n%s", rep.String())

	if rep.IndexedP95Millis > rep.ScanP95Millis {
		t.Errorf("indexed SEARCH p95 %.2f ms is slower than scan p95 %.2f ms — index buys nothing",
			rep.IndexedP95Millis, rep.ScanP95Millis)
	}
	if rep.IndexRatio > indexRatioCap {
		t.Errorf("index is %.2fx the corpus, over the %.1fx cap", rep.IndexRatio, indexRatioCap)
	}
	if rep.Hits == 0 {
		t.Fatal("corpus produced no hits — benchmark is not exercising SEARCH")
	}
}

func BenchmarkIndexAndSearch(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if _, err := Run(Config{Root: b.TempDir(), Corpus: 1000, Iterations: 20}); err != nil {
			b.Fatal(err)
		}
	}
}

// A copy is the same message in a second folder: the run must make them, and
// the search must find them there (#1986).
func TestCopiesAreMadeAndSearchable(t *testing.T) {
	rep, err := Run(Config{
		Root:       t.TempDir(),
		Corpus:     300,
		HitEvery:   10,
		Iterations: 5,
		CopyEvery:  5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Copies != 60 {
		t.Fatalf("the run reports %d copies of 300 messages every 5th, want 60", rep.Copies)
	}
	if rep.CopyHits == 0 {
		t.Error("the second folder answers none of the copied hits, so the copies were never indexed")
	}
	if rep.CopyHits > rep.Hits {
		t.Errorf("the second folder answers %d hits, more than the %d in the corpus", rep.CopyHits, rep.Hits)
	}
}

// The knob is off by default: a run without it and a run with -copies=0 index
// the same corpus into the same bytes, or the A arm is not the old arm.
func TestCopiesZeroChangesNothing(t *testing.T) {
	cfg := Config{Root: t.TempDir(), Corpus: 200, HitEvery: 10, Iterations: 3}
	plain, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Root, cfg.CopyEvery = t.TempDir(), 0
	zero, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if zero.Copies != 0 || zero.CopyHits != 0 {
		t.Errorf("-copies=0 made %d copies answering %d hits", zero.Copies, zero.CopyHits)
	}
	if zero.Corpus != plain.Corpus || zero.Hits != plain.Hits {
		t.Errorf("with the knob at zero the run indexed %d messages (%d hits), without it %d (%d)",
			zero.Corpus, zero.Hits, plain.Corpus, plain.Hits)
	}
	// Bytes, within one glass block: where the last commit falls decides a
	// single 8K block, and that is not the knob doing anything.
	if diff := zero.IndexBytes - plain.IndexBytes; diff > 8192 || diff < -8192 {
		t.Errorf("with the knob at zero the index is %d bytes, without it %d", zero.IndexBytes, plain.IndexBytes)
	}
}
