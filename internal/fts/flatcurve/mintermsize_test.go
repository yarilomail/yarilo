//go:build flatcurve

package flatcurve

import (
	"reflect"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/fts"
)

// The minimum term size is a judgement about what is worth indexing, and that
// judgement is the same in every script. Counting bytes made it a different
// rule per script: a one-character Latin word was dropped and a one-character
// Cyrillic or CJK one was kept, because their bytes outnumbered their
// characters (#1055).
//
// Asserted per script rather than as one case, because the defect is invisible
// in any single alphabet - every ASCII example behaves identically under both
// readings.
func TestMinTermSizeCountsCharacters(t *testing.T) {
	for _, tc := range []struct {
		name    string
		term    string
		minSize int
		kept    bool
	}{
		// One character. Under byte counting the first is dropped and the rest
		// are kept, which is the whole defect in four lines.
		{"latin, one character", "a", 2, false},
		{"cyrillic, one character", "я", 2, false},
		{"cjk, one character", "日", 2, false},
		{"greek, one character", "α", 2, false},

		// Two characters, at the threshold.
		{"latin, two characters", "by", 2, true},
		{"cyrillic, two characters", "як", 2, true},

		// A higher threshold widens the asymmetry: under byte counting a
		// four-letter Latin word was dropped while a two-letter Cyrillic one
		// was kept.
		{"latin, four characters at min 4", "word", 4, true},
		{"latin, three characters at min 4", "and", 4, false},
		{"cyrillic, two characters at min 4", "як", 4, false},
		{"cyrillic, four characters at min 4", "мова", 4, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := normTerm(tc.term, tc.minSize)
			if kept := got != ""; kept != tc.kept {
				t.Errorf("normTerm(%q, %d) = %q, want kept=%v (%d characters, %d bytes)",
					tc.term, tc.minSize, got, tc.kept, len([]rune(tc.term)), len(tc.term))
			}
		})
	}
}

// The storage bound stays in bytes, which is correct and worth pinning: it is
// about how much room a term takes, not about whether it is worth having.
func TestMaxTermLengthStaysInBytes(t *testing.T) {
	// Multi-byte characters, well past the byte bound but not past it in
	// characters.
	long := ""
	for len(long) <= maxTermBytes {
		long += "я"
	}
	got := normTerm(long, 2)
	if len(got) > maxTermBytes {
		t.Errorf("term of %d bytes was not truncated to the %d-byte bound", len(got), maxTermBytes)
	}
	if got == "" {
		t.Error("a long term was dropped rather than truncated")
	}
	// And the truncation must not leave a partial character behind.
	for i, r := range got {
		if r == 0xFFFD {
			t.Errorf("truncation left an invalid rune at byte %d", i)
		}
	}
}

// Substring indexing stops emitting suffixes at the same threshold, so it has
// to count the same way. Under byte counting a Cyrillic word emitted suffixes
// down to a single character.
func TestSubstringSuffixesStopAtTheCharacterThreshold(t *testing.T) {
	ui, _ := testEngine(t, testOpts(Options{SubstringSearch: true, MinTermSize: 3, PrefixSearch: "yes"}))
	indexDoc(t, ui, 1, nil, []string{"мова"})

	for _, tc := range []struct {
		fragment string
		found    bool
	}{
		{"мова", true}, // the term
		{"ова", true},  // 3 characters - at the threshold
		{"ва", false},  // 2 characters - below it, 4 bytes
	} {
		res, err := ui.Lookup([]string{inbox.GUID}, bodyQuery(tc.fragment))
		if err != nil {
			t.Fatal(err)
		}
		if found := len(res.DefiniteGUIDs) > 0; found != tc.found {
			t.Errorf("fragment %q found=%v, want %v (%d characters, %d bytes)",
				tc.fragment, found, tc.found, len([]rune(tc.fragment)), len(tc.fragment))
		}
	}
}

// A word whose token is below the minimum never reaches the index, so it
// leaves the query and the answer is a maybe the scan proves; under NOT the
// complement of that superset would be a subset, so it is everything (#2454).
func TestAShortWordLeavesTheQueryAsAMaybe(t *testing.T) {
	ui, _ := testEngine(t, testOpts(Options{MinTermSize: 2, PrefixSearch: "yes"}))
	indexDoc(t, ui, 1, []string{"xtok", "1793573171", "7"}, nil)
	indexDoc(t, ui, 2, []string{"xtok", "1793573171", "1"}, nil)
	indexDoc(t, ui, 3, []string{"xtok", "1793573171", "77"}, nil)

	subject := func(last string, not bool) fts.Query {
		whole := "xtok-1793573171-" + last
		return fts.Query{AndTerms: true, Terms: []fts.Term{{Field: fts.FieldHeader, HdrName: "subject", Not: not,
			Words: []fts.Word{{Variants: []string{whole, "xtok"}}, {Variants: []string{whole, "1793573171"}}, {Variants: []string{whole, last}}}}}}
	}
	for _, tc := range []struct {
		name            string
		q               fts.Query
		definite, maybe []uint32
	}{
		{"a one-character word is dropped, not required", subject("7", false), nil, []uint32{1, 2, 3}},
		{"a one-character word is no prefix of another term", subject("1", false), nil, []uint32{1, 2, 3}},
		{"NOT over a dropped word is everything, verified", subject("7", true), nil, []uint32{1, 2, 3}},
		{"a two-character word stays definite", subject("77", false), []uint32{3}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := ui.Lookup([]string{inbox.GUID}, tc.q)
			if err != nil {
				t.Fatal(err)
			}
			if d, m := uidsOf(res.DefiniteGUIDs), uidsOf(res.MaybeGUIDs); !reflect.DeepEqual(d, tc.definite) || !reflect.DeepEqual(m, tc.maybe) {
				t.Errorf("definite %v maybe %v, want definite %v maybe %v", d, m, tc.definite, tc.maybe)
			}
		})
	}
}
