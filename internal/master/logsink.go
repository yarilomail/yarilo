package master

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/syslog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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

// syslogMax is the longest message sent to the local syslog socket; a longer
// line is cut to it and marked. The figure is the host's syslogd, measured
// before the port uses syslog (#2715).
var syslogMax = 8192

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

// feed reads r line by line until it ends. Each process of the component
// gets its own pipe, and every one of them feeds the same sink.
func (s *sink) feed(r io.Reader) {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			select {
			case s.lines <- line:
			default:
				s.drop(fmt.Errorf("the log queue is full"))
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *sink) writeLoop() {
	defer close(s.done)
	for line := range s.lines {
		if err := s.dst.write(line); err != nil {
			s.drop(err)
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

func (s *sink) drop(err error) {
	s.dropped.Inc()
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

// syslogDest sends each line at the priority of its JSON level.
type syslogDest struct {
	w   *syslog.Writer
	cut prometheus.Counter
}

func openSyslogDest(facility, tag string, cut prometheus.Counter) (*syslogDest, error) {
	w, err := syslog.New(facilities[facility]|syslog.LOG_INFO, tag)
	if err != nil {
		return nil, fmt.Errorf("syslog: %w", err)
	}
	return &syslogDest{w: w, cut: cut}, nil
}

func (d *syslogDest) write(line []byte) error {
	msg := strings.TrimRight(string(line), "\n")
	level := levelOf(msg)
	msg, cut := fitSyslog(msg, syslogMax)
	if cut {
		d.cut.Inc()
	}
	switch level {
	case "ERROR":
		return d.w.Err(msg)
	case "WARN":
		return d.w.Warning(msg)
	case "DEBUG":
		return d.w.Debug(msg)
	default:
		return d.w.Info(msg)
	}
}

func (d *syslogDest) reopen() error { return nil }
func (d *syslogDest) close() error  { return d.w.Close() }

// fitSyslog cuts msg to max bytes, the cut marked at its end with how much
// went. A line is never split into several messages: a reader would take
// each half for an event of its own.
func fitSyslog(msg string, max int) (string, bool) {
	if len(msg) <= max {
		return msg, false
	}
	tail := fmt.Sprintf(" …[truncated %d bytes]", len(msg)-max)
	keep := max - len(tail)
	for keep > 0 && !utf8.RuneStart(msg[keep]) {
		keep--
	}
	return msg[:keep] + tail, true
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
