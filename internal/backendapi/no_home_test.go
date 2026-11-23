package backendapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// A run over many users skips an account that has no mail home yet; the lone
// form keeps its answer, and a home that exists but cannot be read stays a
// failure (#2675, #2591).
func TestAnAccountWithoutAHomeIsSkippedOnlyWhenAsked(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a home whatever its mode")
	}
	for _, tc := range []struct {
		name       string
		home       string // "", "readable", "unreadable"
		skip       bool
		wantCrypt  string // job state
		wantStatus int    // purge
		wantSkip   bool   // purge answered skipped
	}{
		{"no home, many-user form", "", true, "skipped", http.StatusOK, true},
		{"no home, lone form", "", false, "failed", http.StatusNotFound, false},
		{"unreadable home, many-user form", "unreadable", true, "failed", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, root := storageTestServer(t)
			mts, mroot := storageTestServerMdbox(t)
			const user = "alice@example.com"
			if tc.home != "" {
				materialiseHome(t, ts, user)
				materialiseHome(t, mts, user)
			}
			if tc.home == "unreadable" {
				for _, r := range []string{root, mroot} {
					home := filepath.Join(r, "example.com", "alice")
					if err := os.Chmod(home, 0); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.Chmod(home, 0o700) })
				}
			}

			status, body := doJSON(t, ts, http.MethodPost, "/api/backend/crypt/status", "",
				map[string]any{"user": user, "skip_no_home": tc.skip})
			if status != http.StatusAccepted {
				t.Fatalf("crypt status start: %d %s", status, body)
			}
			got := waitCryptJob(t, ts.URL, user)
			if got.State != tc.wantCrypt {
				t.Errorf("crypt job %s (%s), want %s", got.State, got.Error, tc.wantCrypt)
			}

			status, body = doJSON(t, mts, http.MethodPost, "/api/backend/mdbox/purge", "",
				map[string]any{"user": user, "skip_no_home": tc.skip})
			var resp mdboxPurgeResponse
			_ = json.Unmarshal(body, &resp)
			skipped := status == http.StatusOK && resp.Skipped == errNoMailHome.Error()
			if skipped != tc.wantSkip || (tc.wantStatus != 0 && status != tc.wantStatus) {
				t.Errorf("purge: %d %s, want status %d skipped %v", status, body, tc.wantStatus, tc.wantSkip)
			}
		})
	}
}
