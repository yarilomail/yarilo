package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// purgeAPI answers a purge per user from answers: "skip" is a store other
// than mdbox, "fail" a backend error, "unknown" a user the backend does not
// know; anything else is purged with stats taken from the user's number.
func purgeAPI(t *testing.T, users []string, answers map[string]string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/backend/user/iterate":
			_ = json.NewEncoder(w).Encode(map[string]any{"users": users})
		case "/api/backend/mdbox/purge":
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			user, _ := req["user"].(string)
			if req["skip_other_drivers"] != true {
				t.Errorf("purge of %s without skip_other_drivers", user)
			}
			switch answers[user] {
			case "skip":
				_, _ = io.WriteString(w, `{"skipped":"not an mdbox store"}`)
			case "fail":
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error":"purge: disk full"}`)
			case "unknown":
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"no such user"}`)
			default:
				n := len(user)
				_ = json.NewEncoder(w).Encode(map[string]int{
					"files_rewritten": n, "files_unlinked": 1, "records_expunged": 10 * n, "bytes_reclaimed": 1000 * n,
				})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	oldRoute, oldBackend, oldFmt := routeByUser, backendAPIURL, outputFormat
	t.Cleanup(func() { routeByUser, backendAPIURL, outputFormat = oldRoute, oldBackend, oldFmt })
	routeByUser, backendAPIURL, outputFormat = false, ts.URL, "json"
	return ts
}

func purgeSummaryOf(t *testing.T, out string) purgeSummary {
	t.Helper()
	i := strings.Index(out, "{\n")
	if i < 0 {
		t.Fatalf("no summary in %q", out)
	}
	var sum purgeSummary
	if err := json.Unmarshal([]byte(out[i:strings.LastIndex(out, "}")+1]), &sum); err != nil {
		t.Fatalf("summary %q: %v", out[i:], err)
	}
	return sum
}

func TestMdboxPurgeOverManyUsers(t *testing.T) {
	userFile := filepath.Join(t.TempDir(), "users")
	if err := os.WriteFile(userFile, []byte("a@x\n\n# a comment\nghost@x\nccc@x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		args        []string
		answers     map[string]string
		wantErr     string // "" = exit 0
		purged      int
		skipped     []string
		failed      []string
		wantExpunge int
		wantBytes   int64
	}{
		{"an sdbox user is skipped, the totals are the sum", []string{"--all"},
			map[string]string{"bb@x": "skip"}, "", 2, []string{"bb@x"}, []string{}, 30 + 50, 3000 + 5000},
		{"one failure does not stop the rest and is named", []string{"--all", "--jobs", "2"},
			map[string]string{"bb@x": "fail"}, "bb@x", 2, []string{}, []string{"bb@x"}, 80, 8000},
		{"an unknown user in the file fails, the rest run", []string{"--user-file", userFile},
			map[string]string{"ghost@x": "unknown"}, "ghost@x", 2, []string{}, []string{"ghost@x"}, 30 + 50, 3000 + 5000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			purgeAPI(t, []string{"a@x", "bb@x", "ccc@x"}, tc.answers)
			out, err := captureStd(t, func() error { return mdboxPurge(tc.args) })
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("err %v, want exit 0", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err %v, want %s named", err, tc.wantErr)
			}
			sum := purgeSummaryOf(t, out)
			if sum.Users != 3 || sum.Purged != tc.purged ||
				strings.Join(sum.SkippedUsers, ",") != strings.Join(tc.skipped, ",") ||
				strings.Join(sum.FailedUsers, ",") != strings.Join(tc.failed, ",") ||
				sum.Totals.RecordsExpunged != tc.wantExpunge || sum.Totals.BytesReclaimed != tc.wantBytes {
				t.Fatalf("summary %+v", sum)
			}
		})
	}
}

// --jobs bounds the purges in flight: jobs workers start, and the backend never
// sees more requests at once than that.
func TestMdboxPurgeJobsBoundsTheInFlight(t *testing.T) {
	const jobs, users = 2, 6
	var mu sync.Mutex
	inFlight, most := 0, 0
	arrived := make(chan struct{})
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/backend/user/iterate" {
			_, _ = io.WriteString(w, `{"users":["u1","u2","u3","u4","u5","u6"]}`)
			return
		}
		mu.Lock()
		inFlight++
		most = max(most, inFlight)
		mu.Unlock()
		arrived <- struct{}{}
		<-release
		mu.Lock()
		inFlight--
		mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	}))
	defer ts.Close()
	oldRoute, oldBackend, oldFmt := routeByUser, backendAPIURL, outputFormat
	defer func() { routeByUser, backendAPIURL, outputFormat = oldRoute, oldBackend, oldFmt }()
	routeByUser, backendAPIURL, outputFormat = false, ts.URL, "json"

	var workers int
	oldStarted := purgeWorkerStarted
	defer func() { purgeWorkerStarted = oldStarted }()
	purgeWorkerStarted = func() { mu.Lock(); workers++; mu.Unlock() }

	done := make(chan error, 1)
	go func() {
		_, err := captureStd(t, func() error { return mdboxPurge([]string{"--all", "--jobs", "2"}) })
		done <- err
	}()
	for range jobs {
		<-arrived
	}
	for range users - jobs {
		release <- struct{}{}
		<-arrived
	}
	for range jobs {
		release <- struct{}{}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if workers != jobs || most > jobs {
		t.Fatalf("%d workers and %d purges in flight at most, want %d", workers, most, jobs)
	}
}
