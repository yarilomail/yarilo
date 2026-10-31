package search

import (
	"fmt"
	"testing"

	"github.com/emersion/go-imap/v2/imapserver"
)

// A strict rule reads even what the index called definite, as a strict SEARCH
// does: a definite hit the index got wrong is dropped, not kept on its word.
func TestStrictSearchVerifiesDefiniteHits(t *testing.T) {
	f, msgs, box := folderOf(t)
	idx := &fakeIndex{indexed: 3, definite: []uint32{1, 2}}
	o := Options{Client: idx, Chain: chain(t), Enabled: true, Strict: true}
	keep, err := o.Folder(box, "u", f, msgs, "SUBJECT needle")
	if err != nil {
		t.Fatal(err)
	}
	if !keep[1] || keep[2] {
		t.Errorf("kept %v, want 1 and not the wrong definite 2", keep)
	}
	if box.opened != 2 {
		t.Errorf("opened %d bodies, want the two definite hits", box.opened)
	}
}

// A rule on a keyword matches the message carrying it, flags and keywords
// alike, without reading a body.
func TestARuleMatchesKeywords(t *testing.T) {
	f, msgs, box := folderOf(t)
	msgs[1].Keywords = []string{"$Work"}
	msgs[2].Flags = []string{`\Flagged`}
	keep, err := Options{}.Folder(box, "u", f, msgs, `OR KEYWORD $Work FLAGGED`)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(keep) != "map[2:true 3:true]" {
		t.Errorf("kept %v, want 2 and 3", keep)
	}
	if box.opened != 0 {
		t.Errorf("opened %d bodies for flags alone", box.opened)
	}
}

// A criterion reads the body when any branch of it does, however deep.
func TestNeedsBodyLooksInsideOrAndNot(t *testing.T) {
	for rule, want := range map[string]bool{
		"OR SEEN FLAGGED":                  false,
		"OR SEEN SUBJECT x":                true,
		"OR SUBJECT x SEEN":                true,
		"NOT SEEN":                         false,
		"NOT BODY x":                       true,
		"OR SEEN NOT SENTSINCE 1-Jan-2020": true,
	} {
		c, err := imapserver.ParseSearchCriteria(rule)
		if err != nil {
			t.Fatalf("%s: %v", rule, err)
		}
		if got := NeedsBody(c); got != want {
			t.Errorf("%s: needs body %v, want %v", rule, got, want)
		}
	}
}
