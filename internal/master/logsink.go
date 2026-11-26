package master

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/syslog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
)

var facilities = map[string]syslog.Priority{
	"kern": syslog.LOG_KERN, "user": syslog.LOG_USER, "mail": syslog.LOG_MAIL,
	"daemon": syslog.LOG_DAEMON, "auth": syslog.LOG_AUTH, "syslog": syslog.LOG_SYSLOG,
	"local0": syslog.LOG_LOCAL0, "local1": syslog.LOG_LOCAL1, "local2": syslog.LOG_LOCAL2,
	"local3": syslog.LOG_LOCAL3, "local4": syslog.LOG_LOCAL4, "local5": syslog.LOG_LOCAL5,
	"local6": syslog.LOG_LOCAL6, "local7": syslog.LOG_LOCAL7,
}

// syslogDatagram is the largest datagram the local syslog socket takes,
// header and all: on the FreeBSD host 8191 bytes arrived and 8192 was dropped
// whole, not cut (net.local.dgram.maxdgram 8192, measured 2026-11-26). A line
// that would pass it is cut to fit and marked.
var syslogDatagram = 8191

// syslogSockets are where the local syslog listens, by system.
var syslogSockets = []string{"/var/run/log", "/dev/log", "/var/run/syslog"}

// queued is how many lines a sink holds while its destination is slow. Past
// it lines are dropped and counted: a full disk must never block a component
// in write.
const queued = 4096

// destination is where one component's lines end up.
type destination interface {
	write(line []byte) error
	reopen() error
	close() error
}

// sink drains one component's output into its destination. Reading never
// waits on the destination: a line that does not fit the queue is dropped.
type sink struct {
	name    string
	dst     destination
	lines   chan []byte
	dropped prometheus.Counter
	cut     prometheus.Counter
	onDrop  func(name string, dropping bool, err error)
	done    chan struct{}

	mu       sync.Mutex
	dropping bool
}

func newSink(name string, dst destination, m *metrics, onDrop func(string, bool, error)) *sink {
	s := &sink{
		name:    name,
		dst:     dst,
		lines:   make(chan []byte, queued),
		dropped: m.logDropped.WithLabelValues(name),
		cut:     m.logTruncated.WithLabelValues(name),
		onDrop:  onDrop,
		done:    make(chan struct{}),
	}
	go s.writeLoop()
	return s
}

// feed reads r until it ends and hands on whole lines a chunk at a time:
// one queue entry and one write for as many lines as one read brought, not
// one each. A part line waits for its end; one longer than the buffer is
// handed on as it is.
func (s *sink) feed(r io.Reader) {
	const size = 64 << 10
	buf := make([]byte, size)
	have := 0
	for {
		n, err := r.Read(buf[have:])
		have += n
		end := bytes.LastIndexByte(buf[:have], '\n') + 1
		if end == 0 && have == size {
			end = have // a line longer than the buffer
		}
		if err != nil {
			end = have // what is left when the pipe ends
		}
		if end > 0 {
			chunk := make([]byte, end)
			copy(chunk, buf[:end])
			select {
			case s.lines <- chunk:
			default:
				s.drop(fmt.Errorf("the log queue is full"), lineCount(chunk))
			}
			have = copy(buf, buf[end:have])
		}
		if err != nil {
			return
		}
	}
}

// lineCount is how many lines a chunk holds, a part line at its end counted.
func lineCount(chunk []byte) int {
	n := bytes.Count(chunk, []byte{'\n'})
	if len(chunk) > 0 && chunk[len(chunk)-1] != '\n' {
		n++
	}
	return n
}

func (s *sink) writeLoop() {
	defer close(s.done)
	for chunk := range s.lines {
		if err := s.dst.write(chunk); err != nil {
			s.drop(err, lineCount(chunk))
			continue
		}
		s.mu.Lock()
		was := s.dropping
		s.dropping = false
		s.mu.Unlock()
		if was && s.onDrop != nil {
			s.onDrop(s.name, false, nil)
		}
	}
}

func (s *sink) drop(err error, lines int) {
	s.dropped.Add(float64(lines))
	s.mu.Lock()
	was := s.dropping
	s.dropping = true
	s.mu.Unlock()
	if !was && s.onDrop != nil {
		s.onDrop(s.name, true, err)
	}
}

func (s *sink) reopen() error { return s.dst.reopen() }

func (s *sink) close() error {
	close(s.lines)
	<-s.done
	return s.dst.close()
}

// fileDest is <dir>/<name>.log, opened without following a symlink.
type fileDest struct {
	path string
	mu   sync.Mutex
	f    *os.File
}

func openFileDest(dir, name string) (*fileDest, error) {
	d := &fileDest{path: filepath.Join(dir, name+".log")}
	if err := d.reopen(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *fileDest) reopen() error {
	f, err := os.OpenFile(d.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o640)
	if err != nil {
		return fmt.Errorf("open %s: %w", d.path, err)
	}
	d.mu.Lock()
	old := d.f
	d.f = f
	d.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return nil
}

func (d *fileDest) write(line []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.f.Write(line)
	return err
}

func (d *fileDest) close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.f.Close()
}

// syslogDest sends each line as one datagram at the priority of its JSON
// level, in the local format the system's own syslog(3) writes:
// "<pri>Mmm dd hh:mm:ss tag[pid]: msg".
type syslogDest struct {
	conn     net.Conn
	facility syslog.Priority
	tag      string
	max      int // the whole datagram
	cut      prometheus.Counter
	now      func() time.Time
}

func openSyslogDest(facility, tag string, cut prometheus.Counter) (*syslogDest, error) {
	var last error
	for _, path := range syslogSockets {
		c, err := new(net.Dialer).DialContext(context.Background(), "unixgram", path)
		if err == nil {
			return &syslogDest{conn: c, facility: facilities[facility], tag: tag, max: syslogDatagram, cut: cut, now: time.Now}, nil
		}
		last = err
	}
	return nil, fmt.Errorf("syslog: no local socket: %w", last)
}

// write sends each line of the chunk as a datagram of its own.
func (d *syslogDest) write(chunk []byte) error {
	_, err := d.writeFrom(chunk)
	return err
}

// writeFrom sends the chunk's lines in order and stops at the first that
// fails; sent is how many bytes of the chunk went out whole, so a caller
// that dials again resends only what did not (#2725 review).
func (d *syslogDest) writeFrom(chunk []byte) (sent int, err error) {
	for _, line := range bytes.SplitAfter(chunk, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		if err := d.writeLine(line); err != nil {
			return sent, err
		}
		sent += len(line)
	}
	return sent, nil
}

func (d *syslogDest) writeLine(line []byte) error {
	msg := strings.TrimRight(string(line), "\n")
	pri := d.facility | severity(levelOf(msg))
	header := fmt.Sprintf("<%d>%s %s[%d]: ", pri, d.now().Format(time.Stamp), d.tag, os.Getpid())
	msg, cut := fitSyslog(msg, d.max-len(header))
	if cut {
		d.cut.Inc()
	}
	_, err := d.conn.Write([]byte(header + msg))
	return err
}

func severity(level string) syslog.Priority {
	switch level {
	case "ERROR":
		return syslog.LOG_ERR
	case "WARN":
		return syslog.LOG_WARNING
	case "DEBUG":
		return syslog.LOG_DEBUG
	default:
		return syslog.LOG_INFO
	}
}

func (d *syslogDest) reopen() error { return nil }
func (d *syslogDest) close() error  { return d.conn.Close() }

// fitSyslog cuts msg to at most max bytes, its end marked with how many went.
// A line is never split into several messages: a reader would take each half
// for an event of its own.
func fitSyslog(msg string, max int) (string, bool) {
	if len(msg) <= max {
		return msg, false
	}
	keep := max
	for keep > 0 {
		for keep > 0 && !utf8.RuneStart(msg[keep]) {
			keep--
		}
		tail := fmt.Sprintf(" …[truncated %d bytes]", len(msg)-keep)
		if keep+len(tail) <= max {
			return msg[:keep] + tail, true
		}
		keep--
	}
	return "", true
}

// levelOf reads the level of a component's JSON line; a line it cannot read
// goes at info.
func levelOf(line string) string {
	var v struct {
		Level string `json:"level"`
	}
	if json.Unmarshal([]byte(line), &v) != nil {
		return "INFO"
	}
	l := strings.ToUpper(v.Level)
	if i := strings.IndexAny(l, "+-"); i > 0 { // slog's WARN+2 and the like
		l = l[:i]
	}
	return l
}
