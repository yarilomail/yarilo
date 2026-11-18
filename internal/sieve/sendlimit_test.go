package sieve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/dict"
)

// limitEngine is an engine with the send limit at n on the file driver and a
// sender that records every recipient it is handed.
func limitEngine(t *testing.T, n int) (*Engine, *[]string) {
	t.Helper()
	cfg := config.Defaults().Sieve
	cfg.Enabled = true
	cfg.SubmissionHost = "submit.invalid:587"
	cfg.SendRateLimit = n
	e := New(cfg, nil, nil, nil)
	var sent []string
	e.sender.send = func(_ context.Context, _ string, to []string, _ *bytes.Reader) error {
		sent = append(sent, to...)
		return nil
	}
	return e, &sent
}

func withScript(t *testing.T, user, src string) string {
	t.Helper()
	store, home, ctx := newTestStore(), t.TempDir(), context.Background()
	if err := store.SaveScript(ctx, user, home, "main", []byte(src)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetActive(ctx, user, home, "main"); err != nil {
		t.Fatal(err)
	}
	return home
}

func inInbox(r *FilterResult) bool {
	for _, d := range r.Deliveries {
		if d.FromKeep && d.Folder == "INBOX" {
			return true
		}
	}
	return false
}

// N redirects pass, the N+1st is not sent and its message stays in INBOX.
func TestSendLimitRefusesPastNAndKeeps(t *testing.T) {
	e, sent := limitEngine(t, 3)
	home := withScript(t, "u1", `redirect "a@x.test";`)
	refused := testutil.ToFloat64(metricSendRefused.WithLabelValues("redirect"))
	for i := 1; i <= 4; i++ {
		res, err := e.Filter(context.Background(), baseOpts("u1", home))
		if err != nil {
			t.Fatal(err)
		}
		if want := min(i, 3); len(*sent) != want {
			t.Fatalf("message %d: %d sent, want %d", i, len(*sent), want)
		}
		if kept := inInbox(res); kept != (i == 4) {
			t.Fatalf("message %d: in INBOX %v, want %v", i, kept, i == 4)
		}
	}
	if d := testutil.ToFloat64(metricSendRefused.WithLabelValues("redirect")) - refused; d != 1 {
		t.Fatalf("refused +%v, want +1", d)
	}
}

// Every recipient counts: twenty redirects of one message are twenty.
func TestSendLimitCountsEveryRecipient(t *testing.T) {
	e, sent := limitEngine(t, 25)
	var src strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&src, "redirect \"r%d@x.test\";\n", i)
	}
	home := withScript(t, "u1", src.String())
	for range 2 {
		if _, err := e.Filter(context.Background(), baseOpts("u1", home)); err != nil {
			t.Fatal(err)
		}
	}
	if len(*sent) != 25 {
		t.Fatalf("%d sent over two messages of twenty, want 25", len(*sent))
	}
}

// Vacation and redirect share one counter; a vacation refused by the limit
// marks nothing, so the next message after the hour still gets its reply.
func TestSendLimitVacationSharesTheCounterAndMarksNothing(t *testing.T) {
	e, sent := limitEngine(t, 1)
	now := time.Date(2026, 11, 18, 10, 5, 0, 0, time.UTC)
	e.sendLimit.now = func() time.Time { return now }
	home := withScript(t, "u1", `require ["vacation"]; redirect "a@x.test"; vacation "away";`)
	opts := baseOpts("u1", home)
	opts.MsgRaw = []byte("From: sender@example.com\r\nTo: u1@example.com\r\nSubject: Hi\r\n\r\nx\r\n")
	if _, err := e.Filter(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 1 || (*sent)[0] != "a@x.test" {
		t.Fatalf("sent %v, want the redirect alone", *sent)
	}
	// The next hour, with room for both: the reply goes out, which the
	// vacation dedup would have stopped had the refused reply been marked.
	now = now.Add(time.Hour)
	e.sendLimit.limit = 5
	if _, err := e.Filter(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 3 || (*sent)[2] != "sender@example.com" {
		t.Fatalf("next hour sent %v, want the redirect and then the vacation reply", *sent)
	}
}

// Two users count apart.
func TestSendLimitPerUser(t *testing.T) {
	e, sent := limitEngine(t, 1)
	h1 := withScript(t, "u1", `redirect "a@x.test";`)
	h2 := withScript(t, "u2", `redirect "a@x.test";`)
	for _, o := range []FilterOptions{baseOpts("u1", h1), baseOpts("u2", h2)} {
		if _, err := e.Filter(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	if len(*sent) != 2 {
		t.Fatalf("%d sent, want one for each user", len(*sent))
	}
}

// 0 counts nothing and never touches a store.
func TestSendLimitOffTouchesNoStore(t *testing.T) {
	e, sent := limitEngine(t, 0)
	e.sendLimit.counted = func() { t.Fatal("the counter store was used with the limit off") }
	home := withScript(t, "u1", `redirect "a@x.test";`)
	for range 3 {
		if _, err := e.Filter(context.Background(), baseOpts("u1", home)); err != nil {
			t.Fatal(err)
		}
	}
	if len(*sent) != 3 {
		t.Fatalf("%d sent, want 3", len(*sent))
	}
}

// failingCounter is a dict whose counter store is down.
type failingCounter struct{ dict.Dict }

func (failingCounter) Incr(context.Context, *dict.OpSettings, string, int64, time.Duration) (int64, error) {
	return 0, errors.New("store down")
}

// A counter store that cannot answer does not stop delivery: the send goes
// out, with a metric.
func TestSendLimitUnreachableStoreSends(t *testing.T) {
	e, sent := limitEngine(t, 1)
	e.sendLimit.driver, e.sendLimit.dict = "redis", failingCounter{}
	home := withScript(t, "u1", `redirect "a@x.test";`)
	before := testutil.ToFloat64(metricSendLimitUnavailable.WithLabelValues("redirect"))
	for range 2 {
		if _, err := e.Filter(context.Background(), baseOpts("u1", home)); err != nil {
			t.Fatal(err)
		}
	}
	if len(*sent) != 2 {
		t.Fatalf("%d sent, want 2: an unreachable store lets sends through", len(*sent))
	}
	if d := testutil.ToFloat64(metricSendLimitUnavailable.WithLabelValues("redirect")) - before; d != 2 {
		t.Fatalf("unavailable +%v, want +2", d)
	}
}

// A refused redirect after an explicit discard does not bring the keep back.
func TestSendLimitRefusedAfterDiscardStaysDiscarded(t *testing.T) {
	e, _ := limitEngine(t, 1)
	home := withScript(t, "u1", `redirect "a@x.test"; discard;`)
	for i := 0; i < 2; i++ {
		res, err := e.Filter(context.Background(), baseOpts("u1", home))
		if err != nil {
			t.Fatal(err)
		}
		if inInbox(res) {
			t.Fatalf("message %d kept after discard", i+1)
		}
	}
}

// recordingCounter counts in memory and records how it was asked.
type recordingCounter struct {
	dict.Dict
	n    map[string]int64
	keys []string
	ttls []time.Duration
	user []string
}

func (r *recordingCounter) Incr(_ context.Context, set *dict.OpSettings, key string, delta int64, ttl time.Duration) (int64, error) {
	if r.n == nil {
		r.n = map[string]int64{}
	}
	r.n[set.Username+"|"+key] += delta
	r.keys, r.ttls, r.user = append(r.keys, key), append(r.ttls, ttl), append(r.user, set.Username)
	return r.n[set.Username+"|"+key], nil
}

// On the redis driver the count is the dict Counter's: a private key of the
// clock hour, the user's scope, two hours of life.
func TestSendLimitThroughTheDictCounter(t *testing.T) {
	e, sent := limitEngine(t, 1)
	rc := &recordingCounter{}
	e.sendLimit.driver, e.sendLimit.dict = "redis", rc
	e.sendLimit.now = func() time.Time { return time.Date(2026, 11, 18, 10, 5, 0, 0, time.UTC) }
	home := withScript(t, "u1@d.test", `redirect "a@x.test";`)
	for range 2 {
		if _, err := e.Filter(context.Background(), baseOpts("u1@d.test", home)); err != nil {
			t.Fatal(err)
		}
	}
	if len(*sent) != 1 {
		t.Fatalf("%d sent, want 1", len(*sent))
	}
	want := dict.PathPrivate + dict.Escape("u1@d.test") + "/sieve/send-rate/2026111810"
	if rc.keys[0] != want || rc.user[0] != "u1@d.test" || rc.ttls[0] != 2*time.Hour {
		t.Fatalf("incr key %q user %q ttl %v, want %q, u1@d.test, 2h", rc.keys[0], rc.user[0], rc.ttls[0], want)
	}
}
