package imap

import (
	"crypto/tls"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/yarilomail/yarilo/internal/warden"
)

// imapWardenClient is the IMAP-server-wide handle to yarilo-warden used to push
// SELECT events. One connection per process, owned by one writer goroutine.
//
// SELECT does not wait for it. The event is accounting -- the warden records
// the folder so `who` can render it, answers OK and can refuse nothing -- so a
// command hands it over and returns. Doing the round trip on the command's own
// goroutine, under one mutex, put every session's SELECT behind every other
// session's round trip: 774.75 s of a 1072.69 s waiting profile (#1875).
//
// A nil receiver (WardenAddr unset) no-ops every operation.
type imapWardenClient struct {
	addr string
	tls  *tls.Config

	events chan wardenEvent
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once

	// mu guards conn and the dial hold; the writer touches both, Close needs
	// conn too.
	mu        sync.Mutex
	conn      *warden.Conn
	backoff   time.Duration
	dialAfter time.Time
	clock     func() time.Time // test seam
	// dialCounter counts dials, for a row about the hold. Nil in production.
	dialCounter *atomic.Int64
}

type wardenEvent struct {
	sessionID string
	folder    string
}

// defaultWardenQueue is how many events wait for the writer before the oldest
// is dropped. Sized for a burst of SELECTs, not for an outage: an unreachable
// warden is meant to cost stale rows in `who`, never a stalled session.
const defaultWardenQueue = 4096

// wardenExchangeTimeout bounds one event's round trip.
const wardenExchangeTimeout = 5 * time.Second

// The hold after a refused dial, doubling to the cap. Events that arrive
// inside it are dropped and counted, which is what a warden being down costs.
const (
	wardenBackoffMin = 100 * time.Millisecond
	wardenBackoffMax = 5 * time.Second
)

// wardenDropped counts events that never reached the warden, by reason, so a
// stale folder in `who` is visible rather than silent: queue_full when a burst
// outran the writer, unreachable while the warden is down, transport when the
// exchange failed mid-flight.
var wardenDropped = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "yarilo_warden_events_dropped_total",
	Help: "SELECT events that never reached the warden, by reason.",
}, []string{"reason"})

func newImapWardenClient(addr string, tlsCfg *tls.Config, queue int) *imapWardenClient {
	if addr == "" {
		return nil
	}
	if queue <= 0 {
		queue = defaultWardenQueue
	}
	c := &imapWardenClient{
		addr:   addr,
		tls:    tlsCfg,
		events: make(chan wardenEvent, queue),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go c.run()
	return c
}

// PushSelect hands SELECT(sessionID, folder) to the writer and returns. Empty
// folder means UNSELECT. When the queue is full the oldest event is dropped:
// the newest describes the session's state now, and the old one describes
// where it used to be.
func (c *imapWardenClient) PushSelect(sessionID, folder string) {
	if c == nil || sessionID == "" {
		return
	}
	ev := wardenEvent{sessionID: sessionID, folder: folder}
	for {
		select {
		case c.events <- ev:
			return
		default:
		}
		// Make room. A drop here is one row of `who` going stale, counted.
		select {
		case <-c.events:
			wardenDropped.WithLabelValues("queue_full").Inc()
		default:
		}
	}
}

// run owns the connection: it writes each event and reads the answer back.
// The protocol carries no request id, so the answer belongs to the event by
// order alone -- and it must be read, or the socket's buffer fills and the
// writer stops with a queue behind it.
func (c *imapWardenClient) run() {
	defer close(c.done)
	for {
		select {
		case <-c.stop:
			return
		case ev := <-c.events:
			c.deliver(ev)
		}
	}
}

func (c *imapWardenClient) deliver(ev wardenEvent) {
	// While the warden is down a dial is refused at once, so without a hold
	// the writer would dial once per queued event and then once per SELECT --
	// and the warden coming back would meet that at full speed.
	if hold := c.dialHold(); hold {
		wardenDropped.WithLabelValues("unreachable").Inc()
		return
	}
	conn, err := c.connect()
	if err != nil {
		c.dialFailed()
		wardenDropped.WithLabelValues("unreachable").Inc()
		slog.Debug("imap/warden: dial", "err", err)
		return
	}
	c.dialOK()
	// Bounded: a warden that accepts and never answers must cost this event,
	// not every event behind it.
	_ = conn.SetDeadline(time.Now().Add(wardenExchangeTimeout))
	// Select writes the line and reads the answer; the answer is discarded,
	// as nothing in the IMAP path depends on it.
	err = conn.Select(ev.sessionID, ev.folder)
	_ = conn.SetDeadline(time.Time{})
	if err != nil {
		wardenDropped.WithLabelValues("transport").Inc()
		slog.Debug("imap/warden: select", "sess", ev.sessionID, "folder", ev.folder, "err", err)
		c.drop()
	}
}

// dialHold reports whether the writer is still holding off after a refused
// dial; dialFailed and dialOK move the hold.
func (c *imapWardenClient) dialHold() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn == nil && c.now().Before(c.dialAfter)
}

func (c *imapWardenClient) dialFailed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.backoff == 0 {
		c.backoff = wardenBackoffMin
	} else if c.backoff < wardenBackoffMax {
		c.backoff *= 2
	}
	c.dialAfter = c.now().Add(c.backoff)
}

func (c *imapWardenClient) dialOK() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.backoff, c.dialAfter = 0, time.Time{}
}

func (c *imapWardenClient) now() time.Time {
	if c.clock != nil {
		return c.clock()
	}
	return time.Now()
}

// connect returns the live connection, dialling if there is none.
func (c *imapWardenClient) connect() (*warden.Conn, error) {
	c.mu.Lock()
	existing := c.conn
	c.mu.Unlock()
	if existing != nil {
		return existing, nil
	}
	// Dialled outside the mutex: the dial reads the greeting, and holding the
	// mutex across it made Close wait for a warden that had gone quiet.
	if c.dialCounter != nil {
		c.dialCounter.Add(1)
	}
	conn, err := warden.Dial(c.addr, c.tls, wardenExchangeTimeout)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil { // somebody else won; keep theirs
		conn.Close()
		return c.conn, nil
	}
	c.conn = conn
	return conn, nil
}

// drop closes the connection so the next event redials.
func (c *imapWardenClient) drop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

// Close stops the writer and releases the connection. Safe on nil.
func (c *imapWardenClient) Close() {
	if c == nil {
		return
	}
	c.once.Do(func() {
		close(c.stop)
		// The writer may be inside an exchange; dropping the connection ends
		// it, so Close does not wait out a warden that went quiet.
		c.drop()
		select {
		case <-c.done:
		case <-time.After(wardenExchangeTimeout + time.Second):
			slog.Debug("imap/warden: the event writer did not stop in time")
		}
		c.drop()
	})
}

// wardenSessionID returns the warden session id the login pod forwarded in the
// YARILO preamble (SESSION=<id>), captured into s.sid by newSession (#808).
// It is the SAME id the login registered the session under in warden, so a
// SELECT push updates the right session. Empty on a direct (non-preamble)
// backend connect, where PushSelect correctly no-ops.
func (s *session) wardenSessionID() string {
	return s.sid
}

// pushWardenSelect hands SELECT(sessionID, folder) to the warden writer so the
// WHO output renders the currently-SELECTed mailbox. Empty folder is UNSELECT.
// Best-effort; no-op when WardenAddr is unset or the connection did not carry
// an XCLIENT session id.
func (s *session) pushWardenSelect(folder string) {
	id := s.wardenSessionID()
	if id == "" {
		return
	}
	s.srv.wardenClient.PushSelect(id, folder)
}
