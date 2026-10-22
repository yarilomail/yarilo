package jmap

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/internal/userstate/counters"
	"github.com/yarilomail/yarilo/pkg/jmapcore"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// eventSourcePath is where the session resource points clients (RFC 8620 §7.3).
const eventSourcePath = "/jmap/eventsource/"

// pushHub keeps one watcher per account with an open stream in this pod.
type pushHub struct {
	srv  *Server
	poll time.Duration
	// done ends every stream when the server shuts down.
	done chan struct{}

	mu       sync.Mutex
	watchers map[string]*watcher
}

func newPushHub(s *Server, poll time.Duration) *pushHub {
	return &pushHub{srv: s, poll: poll, done: make(chan struct{}), watchers: map[string]*watcher{}}
}

// watcher polls one account's counter and keeps each type's state at the
// counter value it was computed for, so streams of one account share the work.
type watcher struct {
	hub  *pushHub
	user string
	path string

	mu     sync.Mutex
	refs   int
	snap   counters.Snapshot
	states map[counters.Type]cachedState
	subs   map[chan struct{}]struct{}
	stop   chan struct{}
}

type cachedState struct {
	at    uint64
	state string
}

// join returns the account's watcher, starting it on the first stream.
func (p *pushHub) join(user, path string) (*watcher, chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w := p.watchers[user]
	if w == nil {
		snap, err := counters.Read(path)
		if err != nil {
			slog.Warn("jmap: push counter unreadable", "user", user, "err", err)
		}
		w = &watcher{hub: p, user: user, path: path, snap: snap,
			states: map[counters.Type]cachedState{}, subs: map[chan struct{}]struct{}{}, stop: make(chan struct{})}
		p.watchers[user] = w
		go w.run()
	}
	ch := make(chan struct{}, 1)
	w.mu.Lock()
	w.refs++
	w.subs[ch] = struct{}{}
	w.mu.Unlock()
	return w, ch
}

func (p *pushHub) leave(w *watcher, ch chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w.mu.Lock()
	delete(w.subs, ch)
	w.refs--
	last := w.refs == 0
	w.mu.Unlock()
	if last {
		close(w.stop)
		delete(p.watchers, w.user)
	}
}

func (w *watcher) run() {
	t := time.NewTicker(w.hub.poll)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
		}
		snap, err := counters.Read(w.path)
		if err != nil {
			slog.Warn("jmap: push counter unreadable", "user", w.user, "err", err)
			continue
		}
		w.mu.Lock()
		moved := snap.Highest != w.snap.Highest
		if moved {
			w.snap = snap
			for ch := range w.subs {
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		}
		w.mu.Unlock()
	}
}

func (w *watcher) snapshot() counters.Snapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.snap
}

// statesOf answers the current state of each type, computing only those whose
// counter moved since the cached answer.
func (w *watcher) statesOf(ctx context.Context, snap counters.Snapshot, types []counters.Type) (map[counters.Type]string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[counters.Type]string, len(types))
	var stale []counters.Type
	for _, t := range types {
		if c, ok := w.states[t]; ok && c.at == snap.ByType[t] {
			out[t] = c.state
		} else {
			stale = append(stale, t)
		}
	}
	if len(stale) == 0 {
		return out, nil
	}
	h, err := w.hub.srv.opts.Storage.open(w.user, "")
	if err != nil {
		return nil, err
	}
	defer h.close()
	for _, t := range stale {
		st, err := w.hub.srv.typeState(ctx, h, t)
		if err != nil {
			return nil, fmt.Errorf("jmap: %s state: %w", t, err)
		}
		w.states[t] = cachedState{at: snap.ByType[t], state: st}
		out[t] = st
	}
	return out, nil
}

// typeState is the state each type's /get answers with, by the same function.
func (s *Server) typeState(ctx context.Context, h *userHandle, t counters.Type) (string, error) {
	switch t {
	case counters.Email:
		return s.emailState(h)
	case counters.Mailbox:
		list, err := s.mailboxList(h)
		if err != nil {
			return "", err
		}
		return mailboxState(list), nil
	case counters.Thread:
		return h.threadState()
	case counters.Quota:
		_, st, err := s.quotaObjects(h)
		return st, err
	case counters.Identity:
		return identityState(h.identities(h.info.Username)), nil
	case counters.EmailSubmission:
		recs, err := s.readRecords(ctx, h)
		return strconv.FormatUint(recs.State, 10), err
	case counters.SieveScript:
		v, err := s.sieveRead(ctx, h, h.info.Username)
		return v.state, err
	case counters.VacationResponse:
		vv, err := s.vacationRead(ctx, h, h.info.Username)
		return vv.v.state, err
	}
	return "", fmt.Errorf("jmap: no state for %s", t)
}

// pushTypes is every type this server answers, in the order "*" means.
func (s *Server) pushTypes() []counters.Type {
	out := []counters.Type{counters.Email, counters.Mailbox, counters.Thread, counters.Quota}
	if s.opts.Submission != nil {
		out = append(out, counters.Identity, counters.EmailSubmission)
	}
	if s.opts.Sieve != nil {
		out = append(out, counters.SieveScript)
		if s.opts.Sieve.Vacation {
			out = append(out, counters.VacationResponse)
		}
	}
	return out
}

// streamTypes reads the types parameter: a list or "*"; unknown names are
// left out, as types this server does not have.
func (s *Server) streamTypes(param string) []counters.Type {
	known := s.pushTypes()
	if strings.TrimSpace(param) == "*" {
		return known
	}
	var out []counters.Type
	for _, name := range strings.Split(param, ",") {
		t := counters.Type(strings.TrimSpace(name))
		if slices.Contains(known, t) && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// handleEventSource streams StateChange events (RFC 8620 §7.3).
func (s *Server) handleEventSource(w http.ResponseWriter, r *http.Request, id identity) {
	if s.push == nil || s.opts.Storage == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	q := r.URL.Query()
	types := s.streamTypes(q.Get("types"))
	if len(types) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	closeAfter := q.Get("closeafter") == "state"
	ping, _ := strconv.Atoi(q.Get("ping"))

	info, err := s.opts.Storage.ResolveUser(id.user)
	if err != nil {
		jmapcore.WriteProblem(w, http.StatusInternalServerError, "User unavailable")
		return
	}
	path := counters.Path(counters.Dir(info.Home, mailbox.ControlRoot(info)))

	// The stream outlives the server's write timeout by design.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	wt, notify := s.push.join(id.user, path)
	defer s.push.leave(wt, notify)
	metricPushStreams.Inc()
	defer metricPushStreams.Dec()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return
	}

	// Without an id the first look only fixes the base; with one, whatever
	// changed after it is sent at once.
	since := wt.snapshot().Highest
	if v, perr := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64); perr == nil {
		since = v
		select {
		case notify <- struct{}{}:
		default:
		}
	}
	sent := map[counters.Type]string{}

	var pings <-chan time.Time
	if ping > 0 {
		t := time.NewTicker(time.Duration(ping) * time.Second)
		defer t.Stop()
		pings = t.C
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.push.done:
			return
		case <-pings:
			if err := writeEvent(w, rc, "ping", "", map[string]int{"interval": ping}); err != nil {
				return
			}
			metricPushEvents.WithLabelValues("ping").Inc()
		case <-notify:
			snap := wt.snapshot()
			changed := intersect(snap.ChangedAfter(since), types)
			since = snap.Highest
			if len(changed) == 0 {
				continue
			}
			states, err := wt.statesOf(r.Context(), snap, changed)
			if err != nil {
				slog.Warn("jmap: push state", "user", id.user, "err", err)
				continue
			}
			out := map[string]string{}
			for t, st := range states {
				if sent[t] != st {
					out[string(t)] = st
					sent[t] = st
				}
			}
			if len(out) == 0 {
				continue
			}
			change := map[string]any{"@type": "StateChange", "changed": map[string]any{id.user: out}}
			if err := writeEvent(w, rc, "state", strconv.FormatUint(snap.Highest, 10), change); err != nil {
				return
			}
			metricPushEvents.WithLabelValues("state").Inc()
			if closeAfter {
				return
			}
		}
	}
}

func intersect(changed, want []counters.Type) []counters.Type {
	var out []counters.Type
	for _, t := range changed {
		if slices.Contains(want, t) {
			out = append(out, t)
		}
	}
	return out
}

func writeEvent(w http.ResponseWriter, rc *http.ResponseController, event, id string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	var sb strings.Builder
	if id != "" {
		sb.WriteString("id: " + id + "\n")
	}
	sb.WriteString("event: " + event + "\ndata: ")
	sb.Write(b)
	sb.WriteString("\n\n")
	if _, err := w.Write([]byte(sb.String())); err != nil {
		return err
	}
	return rc.Flush()
}
