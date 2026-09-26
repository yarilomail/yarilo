package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
)

// Every flag reaches the backend-api, repeats kept apart, and the user it is
// routed by is the one named.
func TestFTSLookupSendsEveryCriterion(t *testing.T) {
	var got url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_, _ = w.Write([]byte(`{"user":"u1","folder":"Sent","terms":[],"definite":[],"maybe":[]}`))
	}))
	defer ts.Close()
	oldRoute, oldBackend := routeByUser, backendAPIURL
	defer func() { routeByUser, backendAPIURL = oldRoute, oldBackend }()
	routeByUser, backendAPIURL = false, ts.URL

	err := ftsLookup([]string{"u1", "--folder", "Sent", "--header", "Subject:a b",
		"--header", "From:x", "--body", "one", "--text", "two"})
	if err != nil {
		t.Fatal(err)
	}
	want := url.Values{"user": {"u1"}, "folder": {"Sent"}, "header": {"Subject:a b", "From:x"},
		"body": {"one"}, "text": {"two"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sent %v, want %v", got, want)
	}
	if u := resolveBackendUser("/api/backend/fts/lookup?"+want.Encode(), nil); u != "u1" {
		t.Errorf("routed by %q, want u1", u)
	}
}

func TestFTSLookupNeedsACriterion(t *testing.T) {
	if err := ftsLookup([]string{"u1"}); err == nil {
		t.Error("a lookup with no criterion was sent")
	}
}
