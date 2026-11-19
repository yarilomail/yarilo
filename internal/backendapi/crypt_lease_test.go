package backendapi

import (
	"context"
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
		live      bool
		wantState string
		wantError string
	}{
		{"the holder died", false, "done", ""},
		{"a job runs elsewhere", true, "failed", "already running on another backend"},
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
			if st, body := doJSON(t, ts, http.MethodPost, "/api/backend/crypt/status", "", map[string]any{"user": user}); st != http.StatusAccepted {
				t.Fatalf("start: %d %s", st, body)
			}
			v := waitCryptJob(t, ts.URL, user)
			if v.State != tc.wantState || !strings.Contains(v.Error, tc.wantError) {
				t.Fatalf("job ended %s %q, want %s %q", v.State, v.Error, tc.wantState, tc.wantError)
			}
			if took := time.Since(start); tc.live && took < cryptJobLease {
				t.Errorf("refused after %v, before one lease term (%v) had passed", took, cryptJobLease)
			}
		})
	}
}
