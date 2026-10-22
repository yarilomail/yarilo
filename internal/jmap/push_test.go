package jmap

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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

type pushFixture struct {
	s      *Server
	ts     *httptest.Server
	info   *mailbox.UserInfo
	locker *testLocker
	email  string
}

// pushServer is a stored server with push on, reachable over a real socket so
// a stream can be read while it is open.
func pushServer(t *testing.T, poll, writeTimeout time.Duration) *pushFixture {
	t.Helper()
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: testUser, Home: home, Separator: "/"}
	locker := &testLocker{}
	guid := seedMessage(t, info, locker, "Subject: s\r\n\r\nb\r\n")
	s := New(Options{
		Trust:    ResolveTrust(false, true, []*net.IPNet{mustCIDR(t, "127.0.0.0/8"), mustCIDR(t, "192.0.2.0/24")}),
		Limits:   testLimits(),
		PushPoll: poll,
		Storage: &Storage{
			Mailbox:     maildir.New(),
			Index:       file.New(file.WithLocker(locker)),
			ResolveUser: func(string) (*mailbox.UserInfo, error) { return info, nil },
			Locker:      locker,
		},
	})
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.Config.WriteTimeout = writeTimeout
	ts.Config.ReadTimeout = writeTimeout
	ts.Start()
	t.Cleanup(ts.Close)
	return &pushFixture{s: s, ts: ts, info: info, locker: locker, email: hex.EncodeToString(guid[:])}
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
	owner := locks.Owner(testUser, "test")
	c := counters.For(f.info, owner, f.locker)
	if err := subs.New(f.info.Home, "subscriptions", testUser, owner, f.locker, c).Add(folder); err != nil {
		t.Fatal(err)
	}
}

func TestFlagChangeArrivesWithinOnePoll(t *testing.T) {
	f := pushServer(t, testPoll, 0)
	st := f.open(t, "types=Email,Mailbox", "")
	time.Sleep(2 * testPoll) // the base is fixed before the change
	start := time.Now()
	f.flag(t, "$flagged")
	ev, ok := st.next(t, "state", 20*testPoll)
	if !ok {
		t.Fatal("no StateChange after a flag change")
	}
	if took := time.Since(start); took > 3*testPoll {
		t.Errorf("StateChange took %v, more than one poll (%v) plus slack", took, testPoll)
	}
	if ev.data["@type"] != "StateChange" || ev.id == "" {
		t.Errorf("event %+v", ev)
	}
	for _, typ := range []string{"Email", "Mailbox"} {
		if _, ok := changed(ev)[typ]; !ok {
			t.Errorf("%s missing from %v", typ, changed(ev))
		}
	}
}

// A reconnect names the last event it saw and gets only what changed after it.
func TestReconnectSendsOnlyTypesChangedAfterTheID(t *testing.T) {
	f := pushServer(t, testPoll, 0)
	st := f.open(t, "types=Email,Mailbox&closeafter=state", "")
	time.Sleep(2 * testPoll)
	f.flag(t, "$flagged")
	ev, ok := st.next(t, "state", 20*testPoll)
	if !ok {
		t.Fatal("no first event")
	}
	f.subscribe(t, "Work") // Mailbox only, while disconnected
	again := f.open(t, "types=Email,Mailbox", ev.id)
	ev2, ok := again.next(t, "state", 20*testPoll)
	if !ok {
		t.Fatal("no event on reconnect")
	}
	got := changed(ev2)
	if _, ok := got["Mailbox"]; !ok || len(got) != 1 {
		t.Errorf("reconnect sent %v, want Mailbox alone", got)
	}
}

func TestCloseAfterStateEndsTheStream(t *testing.T) {
	f := pushServer(t, testPoll, 0)
	st := f.open(t, "types=Email&closeafter=state", "")
	time.Sleep(2 * testPoll)
	f.flag(t, "$flagged")
	if _, ok := st.next(t, "state", 20*testPoll); !ok {
		t.Fatal("no event")
	}
	select {
	case _, open := <-st.events:
		if open {
			t.Error("an event after the one closeafter=state allows")
		}
	case <-time.After(20 * testPoll):
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
	f := pushServer(t, testPoll, 0)
	st := f.open(t, "types=Email", "")
	time.Sleep(2 * testPoll)
	f.subscribe(t, "Work") // Mailbox only
	time.Sleep(4 * testPoll)
	f.flag(t, "$flagged") // Email and Mailbox
	ev, ok := st.next(t, "state", 20*testPoll)
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
	f := pushServer(t, testPoll, 200*time.Millisecond)
	st := f.open(t, "types=Email", "")
	time.Sleep(600 * time.Millisecond)
	f.flag(t, "$flagged")
	if _, ok := st.next(t, "state", 20*testPoll); !ok {
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
