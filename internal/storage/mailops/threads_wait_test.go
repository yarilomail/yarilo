package mailops

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/internal/userstate/threads"
	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/locks/lockstest"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// holdTimer is a real locker that records how long each thread lock is held.
type holdTimer struct {
	locks.Locker
	mu    sync.Mutex
	taken map[string]time.Time
	holds []time.Duration
}

func (h *holdTimer) Lock(ctx context.Context, resource, owner string, ttl time.Duration) (locks.Lock, error) {
	l, err := h.Locker.Lock(ctx, resource, owner, ttl)
	if err == nil && strings.HasPrefix(resource, "threads:") {
		h.mu.Lock()
		h.taken[l.ID] = time.Now()
		h.mu.Unlock()
	}
	return l, err
}

func (h *holdTimer) Unlock(ctx context.Context, id string) error {
	h.mu.Lock()
	if at, ok := h.taken[id]; ok {
		h.holds = append(h.holds, time.Since(at))
		delete(h.taken, id)
	}
	h.mu.Unlock()
	return h.Locker.Unlock(ctx, id)
}

// Sessions of one account recording at once through the lock service: each
// queues for the account's thread lock instead of being turned away by the
// one holding it, and every message lands in its conversation. The hold per
// record is what the queue's 2 s bound is measured against.
func TestConcurrentRecordsOfOneAccountAreAllThreaded(t *testing.T) {
	const sessions, each = 16, 25
	home := t.TempDir()
	lk := &holdTimer{Locker: lockstest.Server(t)(), taken: map[string]time.Time{}}
	user := &mailbox.UserInfo{Username: "u@example.com", Home: home}
	recorder := threads.NewRecorder(nil)
	failed := testutil.ToFloat64(metricThreadRecordFailed)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for s := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A session of its own, as each IMAP session holds: LockID fills
			// its field on first use.
			sess := &mailbox.UserInfo{Username: user.Username, Home: home, SessionID: fmt.Sprint("s", s)}
			o := &Ops{cfg: Config{Protocol: "test", Threads: recorder, Locker: lk}, user: sess}
			<-start
			for i := range each {
				raw := []byte(fmt.Sprintf("Message-ID: <%d.%d@x>\r\nSubject: s%d\r\n\r\n", s, i, s))
				o.recordThread([16]byte{byte(s + 1), byte(i + 1)}, raw)
			}
		}()
	}
	close(start)
	wg.Wait()
	st, err := threads.Load(threads.PathFor(user))
	if err != nil {
		t.Fatal(err)
	}
	missing := 0
	for s := range sessions {
		for i := range each {
			if _, ok := st.ThreadOfGUID(mailbox.FormatObjectID([16]byte{byte(s + 1), byte(i + 1)})); !ok {
				missing++
			}
		}
	}
	if missing > 0 {
		t.Errorf("%d of %d messages recorded at once are outside their conversation", missing, sessions*each)
	}
	if got := testutil.ToFloat64(metricThreadRecordFailed) - failed; got != 0 {
		t.Errorf("%v thread records failed", got)
	}
	lk.mu.Lock()
	holds := slices.Clone(lk.holds)
	lk.mu.Unlock()
	slices.Sort(holds)
	if len(holds) == 0 {
		t.Fatal("no thread lock was held, so this row proves nothing")
	}
	t.Logf("thread lock held per record: %d holds, p50 %v, p99 %v, max %v",
		len(holds), holds[len(holds)/2], holds[len(holds)*99/100], holds[len(holds)-1])
}

// Whole saves from sessions of one account at once: every message is threaded.
func TestConcurrentSavesOfOneAccountAreAllThreaded(t *testing.T) {
	const sessions, each = 8, 10
	home := t.TempDir()
	lk := &holdTimer{Locker: lockstest.Server(t)(), taken: map[string]time.Time{}}
	info := &mailbox.UserInfo{Username: "u@example.com", Home: home, Driver: "maildir"}
	if err := maildir.New().OpenUser(info).Init(); err != nil {
		t.Fatal(err)
	}
	recorder := threads.NewRecorder(nil)
	failed := testutil.ToFloat64(metricThreadRecordFailed)
	var wg sync.WaitGroup
	guids := make(chan [16]byte, sessions*each)
	for s := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess := &mailbox.UserInfo{Username: info.Username, Home: home, Driver: "maildir", SessionID: fmt.Sprint("s", s)}
			store, idx := maildir.New().OpenUser(sess), file.New().OpenUser(sess)
			defer func() { _ = store.Close(); _ = idx.Close() }()
			box := mailboxbase.Open(store, idx)
			o := New(Config{Protocol: "test", Threads: recorder, Locker: lk}, box, sess)
			for i := range each {
				body := fmt.Sprintf("Message-ID: <%d.%d@x>\r\nSubject: s%d\r\n\r\nbody\r\n", s, i, s)
				saved, err := o.Save(box, "INBOX", nil, SaveRequest{Body: strings.NewReader(body), Size: int64(len(body))})
				if err != nil {
					t.Error(err)
					return
				}
				guids <- saved.Meta.GUID
			}
		}()
	}
	wg.Wait()
	close(guids)
	st, err := threads.Load(threads.PathFor(info))
	if err != nil {
		t.Fatal(err)
	}
	missing := 0
	for g := range guids {
		if _, ok := st.ThreadOfGUID(mailbox.FormatObjectID(g)); !ok {
			missing++
		}
	}
	if missing > 0 {
		t.Errorf("%d of %d messages saved at once are outside their conversation", missing, sessions*each)
	}
	if got := testutil.ToFloat64(metricThreadRecordFailed) - failed; got != 0 {
		t.Errorf("%v thread records failed", got)
	}
	lk.mu.Lock()
	holds := slices.Clone(lk.holds)
	lk.mu.Unlock()
	slices.Sort(holds)
	if len(holds) == 0 {
		t.Fatal("no thread lock was held, so this row proves nothing")
	}
	t.Logf("thread lock held per save: %d holds, p50 %v, p99 %v, max %v",
		len(holds), holds[len(holds)/2], holds[len(holds)*99/100], holds[len(holds)-1])
}

// A reply recorded before the message it answers, which a refused lock left
// out until ThreadLate records it: both end in one conversation.
func TestAReplyRecordedBeforeItsParentJoinsItWhenTheParentIsThreadedLate(t *testing.T) {
	a := newThreadAccount(t)
	a.lock.refuse = true
	parent := a.save(t, "Message-ID: <p@x>\r\nSubject: s")
	if _, ok := a.threadOf(t, parent); ok {
		t.Fatal("the parent was threaded under a refused lock, so this row proves nothing")
	}
	a.lock.refuse = false
	reply := a.save(t, "Message-ID: <r@x>\r\nIn-Reply-To: <p@x>\r\nReferences: <p@x>\r\nSubject: Re: s")
	if _, err := a.o.ThreadLate("INBOX", []*mailbox.MessageMeta{parent}); err != nil {
		t.Fatal(err)
	}
	pt, ok1 := a.threadOf(t, parent)
	rt, ok2 := a.threadOf(t, reply)
	if !ok1 || !ok2 || pt != rt {
		t.Errorf("parent in %q (%v), reply in %q (%v): want one conversation", pt, ok1, rt, ok2)
	}
}

// busyGate holds the thread lock for someone else while busy is set: Lock
// answers ErrBusy, as the lock service does for a lock another owner holds.
type busyGate struct {
	lockstest.Recorder
	busy bool
}

func (g *busyGate) Lock(ctx context.Context, resource, owner string, ttl time.Duration) (locks.Lock, error) {
	if g.busy && strings.HasPrefix(resource, "threads:") {
		return locks.Lock{}, locks.ErrBusy
	}
	return g.Recorder.Lock(ctx, resource, owner, ttl)
}

// A thread lock held past the wait: the save queues for the bound and no
// longer, stays saved, logs the WARN and counts it; the first read that asks
// for its thread id records it.
func TestASaveQueuesUpToTheBoundThenLeavesTheMessageForThreadLate(t *testing.T) {
	a := newThreadAccount(t)
	g := &busyGate{}
	a.o.cfg.Locker = g
	// The account's sidecar exists: without one ThreadLate leaves the account
	// to the migration.
	a.save(t, "Message-ID: <first@x>\r\nSubject: f")
	g.busy = true
	var buf syncLog
	prev := slogDefault()
	setSlog(&buf)
	t.Cleanup(func() { restoreSlog(prev) })
	failed := testutil.ToFloat64(metricThreadRecordFailed)
	start := time.Now()
	m := a.save(t, "Message-ID: <b@x>\r\nSubject: s")
	took := time.Since(start)
	if took < threadLockWait || took > threadLockWait+2*time.Second {
		t.Errorf("the save took %v under a held lock, want about the %v bound", took, threadLockWait)
	}
	if _, ok := a.threadOf(t, m); ok {
		t.Fatal("threaded under a held lock, so this row proves nothing")
	}
	if got := testutil.ToFloat64(metricThreadRecordFailed) - failed; got != 1 {
		t.Errorf("thread record failures rose by %v, want 1", got)
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "message saved but not threaded") {
		t.Errorf("no WARN for the message left out:\n%s", buf.String())
	}
	g.busy = false
	if _, err := a.o.ThreadLate("INBOX", []*mailbox.MessageMeta{m}); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.threadOf(t, m); !ok {
		t.Error("ThreadLate did not record the message left out")
	}
}

// waitCounter is a real locker that counts the requests made through the
// waiting path, and passes them on.
type waitCounter struct {
	locks.Locker
	mu      sync.Mutex
	waiting int
}

func (w *waitCounter) LockWaiting(ctx context.Context, resource, owner string, ttl, limit time.Duration, shared bool) (locks.Lock, error) {
	w.mu.Lock()
	w.waiting++
	w.mu.Unlock()
	return w.Locker.(interface {
		LockWaiting(context.Context, string, string, time.Duration, time.Duration, bool) (locks.Lock, error)
	}).LockWaiting(ctx, resource, owner, ttl, limit, shared)
}

// A free thread lock is taken with one plain request, never through the
// waiting path, which cost an IMAP save tens of milliseconds it had no reason
// to pay; a held one still queues and the message is threaded.
func TestAFreeThreadLockNeverTakesTheWaitingPath(t *testing.T) {
	lk := &waitCounter{Locker: lockstest.Server(t)()}
	user := &mailbox.UserInfo{Username: "u@example.com", Home: t.TempDir()}
	o := &Ops{cfg: Config{Protocol: "test", Threads: threads.NewRecorder(nil), Locker: lk}, user: user}
	const n = 50
	start := time.Now()
	for i := range n {
		o.recordThread([16]byte{1, byte(i + 1)}, []byte(fmt.Sprintf("Message-ID: <f%d@x>\r\nSubject: s\r\n\r\n", i)))
	}
	per := time.Since(start) / n
	if lk.waiting != 0 {
		t.Errorf("%d records of a free lock went through the waiting path, want 0", lk.waiting)
	}
	t.Logf("a record with the lock free: %v each over %d", per, n)

	// Held by someone else: the record queues behind it and lands once it is
	// released.
	ctx := locks.WithSite(context.Background(), "test")
	held, err := lk.Lock(ctx, locks.ThreadsKey(user.Username), locks.Owner("other@example.com", locks.NewID()), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = lk.Unlock(ctx, held.ID)
	}()
	o.recordThread([16]byte{2}, []byte("Message-ID: <held@x>\r\nSubject: s\r\n\r\n"))
	if lk.waiting != 1 {
		t.Errorf("a held lock made %d waiting requests, want 1", lk.waiting)
	}
	st, err := threads.Load(threads.PathFor(user))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.ThreadOfGUID(mailbox.FormatObjectID([16]byte{2})); !ok {
		t.Error("the record that queued behind a held lock is outside its conversation")
	}
}
