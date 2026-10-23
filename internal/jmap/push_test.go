package jmap

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/userstate/counters"
	"github.com/yarilomail/yarilo/internal/userstate/subs"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

const testPoll = 50 * time.Millisecond

// eventWait bounds an event-driven change; the recompute interval is an hour.
const eventWait = time.Second

type pushFixture struct {
	s      *Server
	ts     *httptest.Server
	info   *mailbox.UserInfo
	locker *testLocker
	// events is the lock service client the server writes and listens through.
	events locks.Locker
	socket string
	email  string
}

// lockService runs an embedded lock service and returns its socket.
func lockService(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "yj")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "l.sock")
	ln, err := locks.ListenUnix(socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := locks.NewServer(locks.NewMemoryBackend(), slog.New(slog.DiscardHandler), nil)
	go srv.Serve(ctx, ln) //nolint:errcheck
	t.Cleanup(cancel)
	return socket
}

func lockClient(t testing.TB, socket string) locks.Locker {
	t.Helper()
	c, err := locks.NewClient(context.Background(), locks.DialUnix(socket))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// pushServer is a stored server with push on, reachable over a real socket so
// a stream can be read while it is open.
func pushServer(t *testing.T, poll, writeTimeout time.Duration) *pushFixture {
	t.Helper()
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: testUser, Home: home, Separator: "/"}
	locker := &testLocker{}
	guid := seedMessage(t, info, locker, "Subject: s\r\n\r\nb\r\n")
	box := maildir.New().OpenUser(info)
	if err := box.Create("Work"); err != nil { // a folder to subscribe
		t.Fatal(err)
	}
	_ = box.Close()
	socket := lockService(t)
	events := lockClient(t, socket)
	s := New(Options{
		Trust:    ResolveTrust(false, true, []*net.IPNet{mustCIDR(t, "127.0.0.0/8"), mustCIDR(t, "192.0.2.0/24")}),
		Limits:   testLimits(),
		PushPoll: poll,
		Storage: &Storage{
			Mailbox:     maildir.New(),
			Index:       file.New(file.WithLocker(locker)),
			ResolveUser: func(string) (*mailbox.UserInfo, error) { return info, nil },
			Locker:      events,
		},
	})
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.Config.WriteTimeout = writeTimeout
	ts.Config.ReadTimeout = writeTimeout
	ts.Start()
	t.Cleanup(ts.Close)
	return &pushFixture{s: s, ts: ts, info: info, locker: locker, events: events, socket: socket, email: hex.EncodeToString(guid[:])}
}

type sse struct {
	id, event string
	data      map[string]any
}

type stream struct {
	resp   *http.Response
	events chan sse
}

func (f *pushFixture) open(t *testing.T, query, lastID string) *stream {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, f.ts.URL+eventSourcePath+"?"+query, nil)
	req.Header.Set(hdrUser, testUser)
	req.Header.Set(hdrSessionID, "deadbeefdeadbeef")
	req.Header.Set(hdrProxyTTL, "4")
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	st := &stream{resp: resp, events: make(chan sse, 16)}
	t.Cleanup(func() { resp.Body.Close() }) //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		close(st.events)
		return st
	}
	go func() {
		defer close(st.events)
		sc := bufio.NewScanner(resp.Body)
		var cur sse
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				st.events <- cur
				cur = sse{}
			case strings.HasPrefix(line, "id: "):
				cur.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				cur.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.data)
			}
		}
	}()
	return st
}

// next waits for the next event of the named kind, skipping others.
func (st *stream) next(t *testing.T, event string, within time.Duration) (sse, bool) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case ev, ok := <-st.events:
			if !ok {
				return sse{}, false
			}
			if ev.event == event {
				return ev, true
			}
		case <-deadline:
			return sse{}, false
		}
	}
}

func changed(ev sse) map[string]any {
	c, _ := ev.data["changed"].(map[string]any)
	m, _ := c[testUser].(map[string]any)
	return m
}

// flag sets a keyword the way a client does, through the mail store.
func (f *pushFixture) flag(t *testing.T, keyword string) {
	t.Helper()
	args := `{"accountId":"` + testUser + `","update":{"` + f.email + `":{"keywords/` + keyword + `":true}}}`
	if name, out := callAs(t, f.s, testUser, "Email/set", args); name != "Email/set" || out["updated"] == nil {
		t.Fatalf("Email/set answered %s %v", name, out)
	}
}

func (f *pushFixture) subscribe(t *testing.T, folder string) {
	t.Helper()
	subscribeThrough(t, f.info.Home, f.events, folder)
}

// subscribeThrough subscribes a folder the way IMAP does, through the given
// lock service client.
func subscribeThrough(t testing.TB, home string, l locks.Locker, folder string) {
	t.Helper()
	owner := locks.Owner(testUser, "test")
	if err := subs.New(home, "subscriptions", testUser, owner, l).Add(folder); err != nil {
		t.Fatal(err)
	}
}

// The recompute is an hour away: the flag's state can only come by event. The
// message is already seen, so Mailbox does not move and is not sent.
func TestFlagChangeArrivesByEvent(t *testing.T) {
	f := pushServer(t, time.Hour, 0)
	st := f.open(t, "types=Email,Mailbox", "")
	f.flag(t, "$flagged")
	ev, ok := st.next(t, "state", eventWait)
	if !ok {
		t.Fatal("no StateChange after a flag change")
	}
	if ev.data["@type"] != "StateChange" || ev.id == "" {
		t.Errorf("event %+v", ev)
	}
	if got := changed(ev); len(got) != 1 || got["Email"] == nil {
		t.Errorf("changed %v, want Email alone", got)
	}
}

// slowSubscribe is a lock service whose subscription takes a while to land.
type slowSubscribe struct{ locks.Locker }

func (l slowSubscribe) Subscribe(ctx context.Context, resource string) (<-chan locks.Event, error) {
	time.Sleep(300 * time.Millisecond)
	return l.Locker.Subscribe(ctx, resource)
}

// A write right after the stream opens is not lost while the subscription is
// still on its way: the base is taken after it.
func TestWriteRightAfterConnectArrives(t *testing.T) {
	f := pushServer(t, time.Hour, 0)
	f.s.opts.Storage.Locker = slowSubscribe{f.events}
	st := f.open(t, "types=Email", "")
	f.flag(t, "$flagged")
	if _, ok := st.next(t, "state", eventWait); !ok {
		t.Fatal("a write right after connect was lost")
	}
}

// changedFirst is a lock service whose subscription opens with an event
// already waiting, so the watcher wakes the stream before it sends anything.
type changedFirst struct{ locks.Locker }

func (l changedFirst) Subscribe(ctx context.Context, resource string) (<-chan locks.Event, error) {
	in, err := l.Locker.Subscribe(ctx, resource)
	if err != nil {
		return nil, err
	}
	out := make(chan locks.Event, 1)
	out <- locks.Event{Resource: locks.MailboxKey(testUser, "INBOX")}
	go func() {
		defer close(out)
		for e := range in {
			out <- e
		}
	}()
	return out, nil
}

// A reconnect with a change already waiting still gets its full state.
func TestReconnectAfterAChangeIsNotStuck(t *testing.T) {
	f := pushServer(t, time.Hour, 0)
	f.s.opts.Storage.Locker = changedFirst{f.events}
	for i := range 10 {
		st := f.open(t, "types=Email,Mailbox&closeafter=state", "7")
		if _, ok := st.next(t, "state", eventWait); !ok {
			t.Fatalf("reconnect %d: no state", i)
		}
	}
}

// failingFirst is a lock service out of reach for the first subscription.
type failingFirst struct {
	locks.Locker
	tried *atomic.Bool
}

func (l failingFirst) Subscribe(ctx context.Context, resource string) (<-chan locks.Event, error) {
	if !l.tried.Swap(true) {
		return nil, errors.New("lock service out of reach")
	}
	return l.Locker.Subscribe(ctx, resource)
}

// A subscription that failed is asked again, and what was written meanwhile
// arrives once it lands.
func TestFailedSubscriptionIsRetried(t *testing.T) {
	f := pushServer(t, time.Hour, 0)
	f.s.opts.Storage.Locker = failingFirst{f.events, &atomic.Bool{}}
	st := f.open(t, "types=Email", "")
	f.flag(t, "$flagged")
	if _, ok := st.next(t, "state", pushResubscribe+eventWait); !ok {
		t.Fatal("no state after the lock service came back")
	}
}

// switchable is a lock service whose subscriptions fail while down is set.
type switchable struct {
	locks.Locker
	down *atomic.Bool
}

func (l switchable) Subscribe(ctx context.Context, resource string) (<-chan locks.Event, error) {
	if l.down.Load() {
		return nil, errors.New("lock service out of reach")
	}
	return l.Locker.Subscribe(ctx, resource)
}

// pushLogs counts the push messages logged at each level.
type pushLogs struct {
	mu     sync.Mutex
	counts map[string]int
}

func (h *pushLogs) Enabled(context.Context, slog.Level) bool { return true }
func (h *pushLogs) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *pushLogs) WithGroup(string) slog.Handler            { return h }
func (h *pushLogs) Handle(_ context.Context, r slog.Record) error {
	if strings.HasPrefix(r.Message, "jmap: push events") {
		h.mu.Lock()
		h.counts[r.Level.String()]++
		h.mu.Unlock()
	}
	return nil
}

func (h *pushLogs) count(level slog.Level) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counts[level.String()]
}

// An outage of the lock service is logged once when it starts and once when
// it ends, however many retries it spans.
func TestOutageLogsOnceEachWay(t *testing.T) {
	logs := &pushLogs{counts: map[string]int{}}
	prev := slog.Default()
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(prev) })

	f := pushServer(t, time.Hour, 0)
	down := &atomic.Bool{}
	down.Store(true)
	f.s.opts.Storage.Locker = switchable{f.events, down}
	st := f.open(t, "types=Email", "")
	time.Sleep(2*pushResubscribe + pushResubscribe/2) // three failed tries
	down.Store(false)
	f.flag(t, "$flagged")
	if _, ok := st.next(t, "state", pushResubscribe+eventWait); !ok {
		t.Fatal("no state after the lock service came back")
	}
	if w, i := logs.count(slog.LevelWarn), logs.count(slog.LevelInfo); w != 1 || i != 1 {
		t.Errorf("logged %d warnings and %d recoveries, want 1 and 1", w, i)
	}
}

// A write that reached no event still arrives, by the recompute, and not
// before it.
func TestRecomputeCatchesALostEvent(t *testing.T) {
	for _, tc := range []struct {
		poll time.Duration
		want bool
	}{{testPoll, true}, {time.Hour, false}} {
		f := pushServer(t, tc.poll, 0)
		st := f.open(t, "types=Mailbox", "")
		subscribeThrough(t, f.info.Home, f.locker, "Work") // emits nowhere
		if _, got := st.next(t, "state", 20*testPoll); got != tc.want {
			t.Errorf("poll %v: event %v, want %v", tc.poll, got, tc.want)
		}
	}
}

// A reconnect gets every requested type's state, whatever its id says: the id
// may come from a pod the user has since been moved away from.
func TestReconnectSendsEveryRequestedType(t *testing.T) {
	f := pushServer(t, time.Hour, 0)
	for _, id := range []string{"1", "100", "0"} {
		st := f.open(t, "types=Email,Mailbox,Thread,Quota&closeafter=state", id)
		ev, ok := st.next(t, "state", eventWait)
		if !ok {
			t.Fatalf("id %s: no event on reconnect", id)
		}
		got := changed(ev)
		for _, typ := range []string{"Email", "Mailbox", "Thread", "Quota"} {
			if _, ok := got[typ]; !ok {
				t.Errorf("id %s: %s missing from %v", id, typ, got)
			}
		}
	}
}

func TestEventMovesItsTypes(t *testing.T) {
	all := []counters.Type{counters.Email, counters.Mailbox, counters.Thread, counters.Quota}
	for _, tc := range []struct {
		resource string
		want     []counters.Type
	}{
		{locks.MailboxKey(testUser, "INBOX"), all},
		{locks.MailboxListKey(testUser), []counters.Type{counters.Mailbox}},
		{locks.SubscriptionsKey(testUser), []counters.Type{counters.Mailbox}},
		{locks.SieveScriptsKey(testUser), []counters.Type{counters.SieveScript, counters.VacationResponse}},
		{locks.ThreadsKey(testUser), []counters.Type{counters.Thread}},
		{locks.SubmissionsKey(testUser), []counters.Type{counters.EmailSubmission}},
		{"quota:" + testUser, nil},
	} {
		if got := typesOfEvent(locks.Event{Resource: tc.resource}); !slices.Equal(got, tc.want) {
			t.Errorf("%s moves %v, want %v", tc.resource, got, tc.want)
		}
	}
}

const pushChildSocket, pushChildHome = "YARILO_PUSH_CHILD_SOCKET", "YARILO_PUSH_CHILD_HOME"

// Another pod writes: a child process with its own lock service client
// subscribes a folder, and the stream here sends Mailbox and nothing else.
func TestAnotherPodsWriteArrives(t *testing.T) {
	if socket := os.Getenv(pushChildSocket); socket != "" {
		subscribeThrough(t, os.Getenv(pushChildHome), lockClient(t, socket), "Work")
		return
	}
	f := pushServer(t, time.Hour, 0)
	st := f.open(t, "types=Email,Mailbox,Thread,Quota", "")
	child := exec.Command(os.Args[0], "-test.run=^TestAnotherPodsWriteArrives$", "-test.count=1")
	child.Env = append(os.Environ(), pushChildSocket+"="+f.socket, pushChildHome+"="+f.info.Home)
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("the other pod failed: %v\n%s", err, out)
	}
	ev, ok := st.next(t, "state", eventWait)
	if !ok {
		t.Fatal("no StateChange after another pod's write")
	}
	if got := changed(ev); len(got) != 1 || got["Mailbox"] == nil {
		t.Errorf("changed %v, want Mailbox alone", got)
	}
}

func TestCloseAfterStateEndsTheStream(t *testing.T) {
	f := pushServer(t, time.Hour, 0)
	st := f.open(t, "types=Email&closeafter=state", "")
	f.flag(t, "$flagged")
	if _, ok := st.next(t, "state", eventWait); !ok {
		t.Fatal("no event")
	}
	select {
	case _, open := <-st.events:
		if open {
			t.Error("an event after the one closeafter=state allows")
		}
	case <-time.After(eventWait):
		t.Error("the stream stayed open after closeafter=state")
	}
}

func TestPingCarriesTheInterval(t *testing.T) {
	f := pushServer(t, testPoll, 0)
	st := f.open(t, "types=Email&ping=1", "")
	ev, ok := st.next(t, "ping", 3*time.Second)
	if !ok {
		t.Fatal("no ping")
	}
	if ev.data["interval"] != float64(1) {
		t.Errorf("ping data %v", ev.data)
	}
}

// A stream names its types: a change to another type sends nothing, and a
// change touching several sends only the asked ones.
func TestTypesFilter(t *testing.T) {
	f := pushServer(t, time.Hour, 0)
	st := f.open(t, "types=Email", "")
	f.subscribe(t, "Work") // Mailbox only
	f.flag(t, "$flagged")  // Email and Mailbox
	ev, ok := st.next(t, "state", eventWait)
	if !ok {
		t.Fatal("no event")
	}
	got := changed(ev)
	if _, ok := got["Email"]; !ok || len(got) != 1 {
		t.Errorf("first event %v, want Email alone", got)
	}
}

// The server's write and read timeouts would end the stream; they must not.
func TestStreamOutlivesTheWriteTimeout(t *testing.T) {
	f := pushServer(t, time.Hour, 200*time.Millisecond)
	st := f.open(t, "types=Email", "")
	time.Sleep(600 * time.Millisecond)
	f.flag(t, "$flagged")
	if _, ok := st.next(t, "state", eventWait); !ok {
		t.Fatal("the stream did not outlive the write timeout")
	}
}

func TestPushOffAnswersNoContent(t *testing.T) {
	f := pushServer(t, 0, 0)
	st := f.open(t, "types=Email", "")
	if st.resp.StatusCode != http.StatusNoContent {
		t.Errorf("status %d, want 204", st.resp.StatusCode)
	}
}

// A pod shutting down ends its streams, or Shutdown waits them out.
func TestShutdownEndsTheStreams(t *testing.T) {
	f := pushServer(t, testPoll, 0)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	f.s.opts.Addr = addr
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- f.s.Serve(ctx) }()
	for i := 0; i < 300; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.ts = &httptest.Server{URL: "http://" + addr}
	st := f.open(t, "types=Email", "")
	time.Sleep(2 * testPoll)
	start := time.Now()
	cancel()
	select {
	case _, open := <-st.events:
		if open {
			t.Error("an event instead of the end of the stream")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream stayed open after shutdown")
	}
	<-served
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("shutdown took %v: it waited for the stream", took)
	}
}
