package query

import (
	"slices"
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/fts"
)

// chartFilters are the chart's default language_filters.
var chartFilters = []string{"lowercase", "stopwords", "snowball"}

func englishChain(t *testing.T) Expander {
	t.Helper()
	c, err := NewChain(config.FTSConfig{LanguageFilters: chartFilters})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func variants(q fts.Query) [][]string {
	var out [][]string
	for _, term := range q.Terms {
		for _, w := range term.Words {
			out = append(out, w.Variants)
		}
	}
	return out
}

// Body text goes through the language chain, a header value through the data
// chain: the same word is stemmed in one and kept whole in the other.
func TestBodyIsStemmedAndAHeaderIsNot(t *testing.T) {
	chain := englishChain(t)
	body, _ := Build(chain, Criteria{Body: []string{"running"}})
	hdr, _ := Build(chain, Criteria{Header: []Header{{Key: "Subject", Value: "running"}}})
	if b := variants(body); len(b) != 1 || slices.Equal(b[0], []string{"running"}) {
		t.Errorf("body variants %v, want a stemmed variant beside the word", b)
	}
	if h := variants(hdr); len(h) != 1 || !slices.Equal(h[0], []string{"running"}) {
		t.Errorf("header variants %v, want the word alone", h)
	}
	if hdr.Terms[0].Field != fts.FieldHeader || hdr.Terms[0].HdrName != "subject" {
		t.Errorf("header term %+v, want field header, name lowercased", hdr.Terms[0])
	}
}

func TestBuild(t *testing.T) {
	chain := englishChain(t)
	for _, tc := range []struct {
		name       string
		c          Criteria
		terms      int
		impossible bool
		phrase     string
		field      fts.FieldKind
	}{
		{"stopwords only can never match", Criteria{Body: []string{"the"}}, 0, true, "", 0},
		{"one stopword criterion makes the whole AND impossible", Criteria{Body: []string{"the"}, Text: []string{"invoice"}}, 1, true, "", 0},
		{"a header without a value asks only for the field", Criteria{Header: []Header{{Key: "X-Tag"}}}, 1, false, "", fts.FieldHeader},
		{"words with a space keep the phrase", Criteria{Text: []string{"quarterly invoice"}}, 1, false, "quarterly invoice", fts.FieldText},
		{"text is its own field", Criteria{Text: []string{"invoice"}}, 1, false, "", fts.FieldText},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, impossible := Build(chain, tc.c)
			if impossible != tc.impossible || len(q.Terms) != tc.terms || !q.AndTerms {
				t.Fatalf("got %d terms, impossible %v, and %v; want %d, %v, true", len(q.Terms), impossible, q.AndTerms, tc.terms, tc.impossible)
			}
			if tc.terms == 1 && !tc.impossible && (q.Terms[0].Phrase != tc.phrase || q.Terms[0].Field != tc.field) {
				t.Errorf("term %+v, want field %v phrase %q", q.Terms[0], tc.field, tc.phrase)
			}
		})
	}
}

// With no languages configured the chain is English, as the fts service's; a
// German default would stem "laufen" and English does not.
func TestNewChainDefaultsToEnglish(t *testing.T) {
	def, err := NewChain(config.FTSConfig{LanguageFilters: chartFilters})
	if err != nil {
		t.Fatal(err)
	}
	en, err := NewChain(config.FTSConfig{Languages: []string{"en"}, LanguageFilters: chartFilters})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := Build(def, Criteria{Body: []string{"running", "laufen"}})
	b, _ := Build(en, Criteria{Body: []string{"running", "laufen"}})
	if !slices.EqualFunc(variants(a), variants(b), slices.Equal[[]string]) {
		t.Errorf("default %v, English %v", variants(a), variants(b))
	}
}
