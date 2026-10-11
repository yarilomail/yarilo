package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// `director domains move` posts the backend to the domain's move endpoint.
func TestDirectorDomainsMovePostsTheBackend(t *testing.T) {
	var path, backend string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.Method + " " + r.URL.EscapedPath()
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		backend = body["backend"]
		w.Write([]byte(`{"status":"ok"}`)) //nolint:errcheck
	}))
	defer srv.Close()
	old := apiURL
	defer func() { apiURL = old }()
	apiURL = srv.URL

	if err := dispatchDirector([]string{"domains", "move", "d00001.test", "10.1.114.60"}); err != nil {
		t.Fatal(err)
	}
	if path != "POST /api/director/domains/d00001.test/move" || backend != "10.1.114.60" {
		t.Errorf("sent %q with backend %q", path, backend)
	}
	if err := dispatchDirector([]string{"domains", "move", "d00001.test"}); err == nil {
		t.Error("a move with no backend was sent")
	}
}
