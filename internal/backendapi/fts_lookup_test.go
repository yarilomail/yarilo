package backendapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	ftsquery "github.com/yarilomail/yarilo/internal/fts/query"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/fts"
)

func lookupServer(t *testing.T) (*httptest.Server, *fakeFTS, ftsquery.Expander) {
	t.Helper()
	ts, fake, root, s := ftsTestServerOf(t)
	chain, err := ftsquery.NewChain(config.FTSConfig{LanguageFilters: []string{"lowercase", "stopwords", "snowball"}})
	if err != nil {
		t.Fatal(err)
	}
	s.opts.FTSChain = chain
	uc, err := newAdminUserContext(t, ts, root, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(uc.cleanup)
	return ts, fake, chain
}

func lookupPath(v url.Values) string { return "/api/backend/fts/lookup?" + v.Encode() }

// The index is asked with the query SEARCH builds from the same criteria, and
// its answer comes back as the index gave it.
func TestFTSLookupAsksWhatSearchWould(t *testing.T) {
	ts, fake, chain := lookupServer(t)
	fake.result = fts.Result{Definite: []uint32{3, 9}, Maybe: []uint32{12}}
	status, body := doJSON(t, ts, http.MethodGet, lookupPath(url.Values{
		"user": {"alice@example.com"}, "folder": {"INBOX"},
		"header": {"Subject:running late"}, "body": {"invoices"},
	}), "", nil)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	want, _ := ftsquery.Build(chain, ftsquery.Criteria{
		Body:   []string{"invoices"},
		Header: []ftsquery.Header{{Key: "Subject", Value: "running late"}},
	})
	if len(fake.lookups) != 1 || !reflect.DeepEqual(fake.lookups[0], want) {
		t.Errorf("the index was asked %+v, want %+v", fake.lookups, want)
	}
	var r ftsLookupResponse
	decodeJSONBody(t, body, &r)
	if !reflect.DeepEqual(r.Definite, []uint32{3, 9}) || !reflect.DeepEqual(r.Maybe, []uint32{12}) {
		t.Errorf("answered definite %v maybe %v, want [3 9] and [12]", r.Definite, r.Maybe)
	}
	if len(r.Terms) != 2 || r.Terms[0].Field != "body" || r.Terms[1].Field != "header" || r.Terms[1].Header != "subject" {
		t.Errorf("terms %+v, want body then header subject", r.Terms)
	}
}

// A criterion of stopwords only can match nothing: the answer says so and the
// index is not asked, as SEARCH does not ask it.
func TestFTSLookupOfStopwordsDoesNotAskTheIndex(t *testing.T) {
	ts, fake, _ := lookupServer(t)
	status, body := doJSON(t, ts, http.MethodGet, lookupPath(url.Values{
		"user": {"alice@example.com"}, "folder": {"INBOX"}, "body": {"the"},
	}), "", nil)
	var r ftsLookupResponse
	decodeJSONBody(t, body, &r)
	if status != http.StatusOK || !r.Impossible || len(fake.lookups) != 0 {
		t.Errorf("status %d impossible %v lookups %d, want 200, true, 0", status, r.Impossible, len(fake.lookups))
	}
}

func TestFTSLookupRefusals(t *testing.T) {
	ts, _, _ := lookupServer(t)
	for _, tc := range []struct {
		name string
		q    url.Values
	}{
		{"no user", url.Values{"folder": {"INBOX"}, "body": {"x"}}},
		{"no folder", url.Values{"user": {"alice@example.com"}, "body": {"x"}}},
		{"no criterion", url.Values{"user": {"alice@example.com"}, "folder": {"INBOX"}}},
		{"a header without a colon", url.Values{"user": {"alice@example.com"}, "folder": {"INBOX"}, "header": {"Subject"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if status, body := doJSON(t, ts, http.MethodGet, lookupPath(tc.q), "", nil); status != http.StatusBadRequest {
				t.Errorf("status=%d body=%s, want 400", status, body)
			}
		})
	}
}

// Without the chain the query cannot be built as SEARCH builds it, so the
// endpoint refuses rather than ask with other words.
func TestFTSLookupWithoutAChainIsNotOffered(t *testing.T) {
	ts, _, _, s := ftsTestServerOf(t)
	s.opts.FTSChain = nil
	status, _ := doJSON(t, ts, http.MethodGet, lookupPath(url.Values{
		"user": {"alice@example.com"}, "folder": {"INBOX"}, "body": {"x"},
	}), "", nil)
	if status != http.StatusNotImplemented {
		t.Errorf("status=%d, want 501", status)
	}
}
