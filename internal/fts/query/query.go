// Package query turns the full-text part of a search into the engine query.
// IMAP SEARCH and the operator lookup build it here, so both ask the index
// with the same terms.
package query

import (
	"fmt"
	"strings"

	"github.com/yarilomail/yarilo/internal/fts/language"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/fts"
)

// Header is one HEADER criterion; an empty Value asks only that the field exist.
type Header struct {
	Key   string
	Value string
}

// Criteria is the full-text part of a search; every key is ANDed.
type Criteria struct {
	Body   []string
	Text   []string
	Header []Header
}

// Expander expands a search string into engine words.
type Expander interface {
	ExpandSearch(query string) []fts.Word
}

// headerChain is the no-stemming data chain headers are indexed with: a
// stemmed variant of an unstemmed header token would match unrelated words.
var headerChain = mustDataChain()

func mustDataChain() *language.Chain {
	c, err := language.NewDataChain()
	if err != nil {
		panic(fmt.Sprintf("fts/query: header data chain: %v", err))
	}
	return c
}

// NewChain builds the language chain the fts service indexes with, from the
// same fts section: a different set would expand queries into other words.
func NewChain(fc config.FTSConfig) (*language.MultiChain, error) {
	langs := fc.Languages
	if len(langs) == 0 {
		langs = []string{"en"}
	}
	return language.NewMultiChain(langs, fc.LanguageFilters, fc.LanguageFiltersOverride,
		fc.LanguageTokenMaxLen, fc.LanguageAddressMaxLen, fc.DetectionMinRunes)
}

// Build returns the engine query. impossible is true when a criterion expanded
// to nothing (only stopwords, which are never indexed): it can never match, so
// neither can the whole ANDed query.
func Build(chain Expander, c Criteria) (q fts.Query, impossible bool) {
	var terms []fts.Term
	add := func(exp Expander, field fts.FieldKind, hdrName, value string) {
		words := exp.ExpandSearch(value)
		if len(words) == 0 && value != "" {
			impossible = true
			return
		}
		t := fts.Term{Field: field, HdrName: hdrName, Words: words}
		if strings.ContainsRune(strings.TrimSpace(value), ' ') {
			t.Phrase = value
		}
		terms = append(terms, t)
	}
	for _, v := range c.Body {
		add(chain, fts.FieldBody, "", v)
	}
	for _, v := range c.Text {
		add(chain, fts.FieldText, "", v)
	}
	for _, h := range c.Header {
		if h.Value == "" {
			terms = append(terms, fts.Term{Field: fts.FieldHeader, HdrName: strings.ToLower(h.Key)})
			continue
		}
		add(headerChain, fts.FieldHeader, strings.ToLower(h.Key), h.Value)
	}
	return fts.Query{Terms: terms, AndTerms: true}, impossible
}
