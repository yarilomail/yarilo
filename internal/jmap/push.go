package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/userstate/counters"
	"github.com/yarilomail/yarilo/v2/pkg/jmapcore"
	"github.com/yarilomail/yarilo/v2/pkg/locks"
)

// eventSourcePath is where the session resource points clients (RFC 8620 §7.3).
const eventSourcePath = "/jmap/eventsource/"

// pushHub keeps one watcher per account with an open stream in this pod.
type pushHub struct {
	srv *Server
	// poll is the safety recompute interval.
	poll time.Duration
	// done ends every stream when the server shuts down.
	done chan struct{}
	// seq names state events; an id is only ever compared by the client.
	seq atomic.Uint64
	// down is set while subscriptions fail, so an outage logs once, not per
	// account per retry.
	down atomic.Bool

	mu       sync.Mutex
	watchers map[string]*watcher
}

func newPushHub(s *Server, poll time.Duration) *pushHub {
	return &pushHub{srv: s, poll: poll, done: make(chan struct{}), watchers: map[string]*watcher{}}
}

// watcher hears one account's events and tells its streams which types moved.
type watcher struct {
	hub  *pushHub
	user string

	mu     sync.Mutex
	refs   int
	subs   map[chan []counters.Type]struct{}
	cancel context.CancelFunc
	// ready closes once the first subscription is asked, so a base taken
	// after it misses no write.
	ready chan struct{}
}

// join returns the account's watcher, starting it on the first stream.
func (p *pushHub) join(user string) (*watcher, chan []counters.Type) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w := p.watchers[user]
	if w == nil {
		ctx, cancel := context.WithCancel(context.Background())
		w = &watcher{hub: p, user: user, subs: map[chan []counters.Type]struct{}{}, cancel: cancel, ready: make(chan struct{})}
		p.watchers[user] = w
		go w.run(ctx)
	}
	ch := make(chan []counters.Type, 1)
	w.mu.Lock()
	w.refs++
	w.subs[ch] = struct{}{}
	w.mu.Unlock()
	return w, ch
}

func (p *pushHub) leave(w *watcher, ch chan []counters.Type) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w.mu.Lock()
	delete(w.subs, ch)
	w.refs--
	last := w.refs == 0
	w.mu.Unlock()
	if last {
		w.cancel()
		delete(p.watchers, w.user)
	}
}

// pushResubscribe is the pause before the event channel is asked again.
const pushResubscribe = 2 * time.Second

func (w *watcher) run(ctx context.Context) {
	t := time.NewTicker(w.hub.poll)
	defer t.Stop()
	lost := false
	for ctx.Err() == nil {
		var events <-chan locks.Event
		err := errors.New("no lock service")
		if l := w.hub.srv.opts.Storage.Locker; l != nil {
			events, err = l.Subscribe(ctx, locks.AccountKey(w.user))
		}
		if err != nil {
			if w.hub.down.CompareAndSwap(false, true) {
				slog.Warn("jmap: push events unavailable, streams fall back to the recompute", "user", w.user, "err", err)
			}
		} else if w.hub.down.CompareAndSwap(true, false) {
			slog.Info("jmap: push events back", "user", w.user)
		}
		if err == nil && lost {
			// Events may have been missed while the channel was down.
			w.notify(allTypes)
		}
		lost = true
		w.markReady()
		w.listen(ctx, events, t.C)
	}
}

func (w *watcher) markReady() {
	select {
	case <-w.ready:
	default:
		close(w.ready)
	}
}

// listen serves the event channel and the safety interval; once the channel
// is missing or closed, it returns after pushResubscribe for another try.
func (w *watcher) listen(ctx context.Context, events <-chan locks.Event, tick <-chan time.Time) {
	var retry <-chan time.Time
	if events == nil {
		retry = time.After(pushResubscribe)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-retry:
			return
		case <-tick:
			w.notify(allTypes)
		case evt, ok := <-events:
			if !ok {
				events, retry = nil, time.After(pushResubscribe)
				continue
			}
			if types := typesOfEvent(evt); len(types) > 0 {
				w.notify(types)
			}
		}
	}
}

// allTypes stands for every type: a recompute, or events that may be lost.
var allTypes = []counters.Type{counters.Email, counters.Mailbox, counters.Thread, counters.Quota,
	counters.Identity, counters.EmailSubmission, counters.SieveScript, counters.VacationResponse}

func (w *watcher) notify(types []counters.Type) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for ch := range w.subs {
		select {
		case ch <- types:
		default:
			// The stream has not read the last one: it reads everything instead.
			select {
			case <-ch:
			default:
			}
			ch <- allTypes
		}
	}
}

// typesOfEvent maps an event to the types whose state it can move, by the key
// it was emitted on.
func typesOfEvent(evt locks.Event) []counters.Type {
	prefix, _, _ := strings.Cut(evt.Resource, ":")
	switch prefix {
	case "mbox":
		return counters.Mail
	case "mlist", "subs":
		return []counters.Type{counters.Mailbox}
	case "sieve":
		return []counters.Type{counters.SieveScript, counters.VacationResponse}
	case "threads":
		return []counters.Type{counters.Thread}
	case "jmapsubmissions":
		return []counters.Type{counters.EmailSubmission}
	case "jmapidentities":
		return []counters.Type{counters.Identity}
	}
	return nil
}

// statesOf answers the current state of each type, by the same functions /get
// answers with.
func (s *Server) statesOf(ctx context.Context, user string, types []counters.Type) (map[counters.Type]string, error) {
	h, err := s.opts.Storage.open(user, "")
	if err != nil {
		return nil, err
	}
	defer h.close()
	out := make(map[counters.Type]string, len(types))
	for _, t := range types {
		st, err := s.typeState(ctx, h, t)
		if err != nil {
			return nil, fmt.Errorf("jmap: %s state: %w", t, err)
		}
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
		all, err := h.identities()
		return identityState(all), err
	case counters.EmailSubmission:
		recs, err := s.readRecords(ctx, h)
		return strconv.FormatUint(recs.State, 10), err
	case counters.SieveScript:
		v, err := s.sieveRead(ctx, h, h.user)
		return v.state, err
	case counters.VacationResponse:
		vv, err := s.vacationRead(ctx, h, h.user)
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

	// The stream outlives the server's write timeout by design.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	wt, notify := s.push.join(id.user)
	defer s.push.leave(wt, notify)
	metricPushStreams.Inc()
	defer metricPushStreams.Dec()
	select {
	case <-wt.ready:
	case <-r.Context().Done():
		return
	case <-s.push.done:
		return
	}

	// Without an id the states at connect are the base. With one, every
	// requested type's state is sent at once: the id may predate a move.
	sent := map[counters.Type]string{}
	var first []counters.Type
	if _, perr := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64); perr == nil {
		first = types
	} else if base, err := s.statesOf(r.Context(), id.user, types); err == nil {
		sent = base
	} else {
		slog.Warn("jmap: push base state", "user", id.user, "err", err)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return
	}

	var pings <-chan time.Time
	if ping > 0 {
		t := time.NewTicker(time.Duration(ping) * time.Second)
		defer t.Stop()
		pings = t.C
	}
	for {
		moved := first
		first = nil
		if moved == nil {
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
				continue
			case moved = <-notify:
			}
		}
		out := s.newStates(r.Context(), id.user, intersect(moved, types), sent)
		if len(out) == 0 {
			continue
		}
		change := map[string]any{"@type": "StateChange", "changed": map[string]any{jmapcore.AccountID(id.user): out}}
		if err := writeEvent(w, rc, "state", strconv.FormatUint(s.push.seq.Add(1), 10), change); err != nil {
			return
		}
		metricPushEvents.WithLabelValues("state").Inc()
		if closeAfter {
			return
		}
	}
}

// newStates returns the states of types that differ from sent, and records
// them there.
func (s *Server) newStates(ctx context.Context, user string, types []counters.Type, sent map[counters.Type]string) map[string]string {
	if len(types) == 0 {
		return nil
	}
	states, err := s.statesOf(ctx, user, types)
	if err != nil {
		slog.Warn("jmap: push state", "user", user, "err", err)
		return nil
	}
	out := map[string]string{}
	for t, st := range states {
		if prev, ok := sent[t]; !ok || prev != st {
			out[string(t)] = st
			sent[t] = st
		}
	}
	return out
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
