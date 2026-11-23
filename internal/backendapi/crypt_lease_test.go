package backendapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/locks/lockstest"
)

// A job started while a dead pod's lease lives waits it out and runs; one
// started while another backend's job keeps renewing it is refused by name,
// not with a bare "resource is busy" (#2610).
func TestACryptJobWaitsOutADeadPodsLease(t *testing.T) {
	lease, renew := cryptJobLease, cryptJobRenew
	cryptJobLease, cryptJobRenew = 2*time.Second, 500*time.Millisecond
	t.Cleanup(func() { cryptJobLease, cryptJobRenew = lease, renew })
	const user = "alice@example.com"
	for _, tc := range []struct {
		name      string
		kind      string
		live      bool
		wantState string
		wantError string
	}{
		{"the holder died", "reencrypt", false, "done", ""},
		{"a job runs elsewhere", "reencrypt", true, "failed", "already running on another backend"},
		// A status takes no lease: it reads while a rewrite runs elsewhere.
		{"a status beside a running job", "status", true, "done", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newClient := lockstest.Server(t)
			ts, _ := storageTestServer(t, func(o *Options) { o.Locker = newClient() })
			materialiseHome(t, ts, user)
			other := newClient()
			key := locks.CryptJobKey(user)
			if tc.live {
				ctx, stop := context.WithCancel(locks.WithSite(context.Background(), "test-live-pod"))
				held := make(chan struct{})
				go func() {
					_ = locks.WithLock(ctx, other, key, locks.Owner(user, "another-pod"), cryptJobLease, cryptJobRenew,
						func(ctx context.Context) error { close(held); <-ctx.Done(); return nil })
				}()
				<-held
				t.Cleanup(stop)
			} else if _, err := other.Lock(locks.WithSite(context.Background(), "test-dead-pod"), key, locks.Owner(user, "dead-pod"), cryptJobLease); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			if st, body := doJSON(t, ts, http.MethodPost, "/api/backend/crypt/"+tc.kind, "", map[string]any{"user": user, "to": "plain"}); st != http.StatusAccepted {
				t.Fatalf("start: %d %s", st, body)
			}
			v := waitCryptJob(t, ts.URL, user)
			if v.State != tc.wantState || !strings.Contains(v.Error, tc.wantError) {
				t.Fatalf("job ended %s %q, want %s %q", v.State, v.Error, tc.wantState, tc.wantError)
			}
			if took := time.Since(start); tc.live && tc.wantState == "failed" && took < cryptJobLease {
				t.Errorf("refused after %v, before one lease term (%v) had passed", took, cryptJobLease)
			}
		})
	}
}

// On one pod a status runs beside the user's re-encryption, and each is read
// by its kind (#2591, owner 2026-11-23).
func TestAStatusRunsBesideAReencryption(t *testing.T) {
	lease, renew := cryptJobLease, cryptJobRenew
	cryptJobLease, cryptJobRenew = 2*time.Second, 500*time.Millisecond
	t.Cleanup(func() { cryptJobLease, cryptJobRenew = lease, renew })
	const user = "alice@example.com"
	newClient := lockstest.Server(t)
	ts, _ := storageTestServer(t, func(o *Options) { o.Locker = newClient() })
	materialiseHome(t, ts, user)
	// Another pod holds the lease, so this pod's re-encryption stays running.
	ctx, stop := context.WithCancel(locks.WithSite(context.Background(), "test-live-pod"))
	t.Cleanup(stop)
	held := make(chan struct{})
	go func() {
		_ = locks.WithLock(ctx, newClient(), locks.CryptJobKey(user), locks.Owner(user, "another-pod"), cryptJobLease, cryptJobRenew,
			func(ctx context.Context) error { close(held); <-ctx.Done(); return nil })
	}()
	<-held
	if st, body := doJSON(t, ts, http.MethodPost, "/api/backend/crypt/reencrypt", "", map[string]any{"user": user, "to": "plain"}); st != http.StatusAccepted {
		t.Fatalf("reencrypt: %d %s", st, body)
	}
	if st, body := doJSON(t, ts, http.MethodPost, "/api/backend/crypt/status", "", map[string]any{"user": user}); st != http.StatusAccepted {
		t.Fatalf("status beside the re-encryption: %d %s", st, body)
	}
	var v cryptJobView
	st, body := doJSON(t, ts, http.MethodGet, "/api/backend/crypt/job?user="+user+"&kind=status&wait=10s", "", nil)
	if st != http.StatusOK || json.Unmarshal(body, &v) != nil || v.State != "done" {
		t.Fatalf("status job: %d %s", st, body)
	}
	st, body = doJSON(t, ts, http.MethodGet, "/api/backend/crypt/job?user="+user+"&kind=reencrypt", "", nil)
	if st != http.StatusOK || json.Unmarshal(body, &v) != nil || v.State != "running" {
		t.Fatalf("reencrypt job: %d %s, want it still running", st, body)
	}
}
