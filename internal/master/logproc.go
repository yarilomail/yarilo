package master

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// LogFDsEnv names, for the log process, which inherited descriptor carries
// which component's output: "3=master,4=locks,...".
const LogFDsEnv = "YARILO_LOG_FDS"

// The log process's settings come through its environment from the
// supervisor, so the log account never needs to read yarilo.yaml and the
// passwords in it.
const (
	LogPathEnv       = "YARILO_LOG_PATH"
	LogFacilityEnv   = "YARILO_LOG_FACILITY"
	LogPprofEnv      = "YARILO_LOG_PPROF"
	LogPprofBlockEnv = "YARILO_LOG_PPROF_BLOCK_RATE"
	LogPprofMutexEnv = "YARILO_LOG_PPROF_MUTEX_FRACTION"
)

// LogTarget is where the log process writes: a directory, or syslog.
type LogTarget struct {
	Path     string // a directory, or "syslog"
	Facility string
}

// stopDrain is how long the log process keeps reading after SIGTERM: what
// the components wrote before the stop is written, then it exits.
const stopDrain = 300 * time.Millisecond

// RunLog is the log process: the supervisor's own binary in the role "log".
// It reads every pipe named in fds and writes whole lines to each
// component's destination until sigs brings SIGTERM or SIGINT; SIGHUP
// reopens the files.
func RunLog(target LogTarget, fds string, reg prometheus.Registerer, sigs <-chan os.Signal, logger *slog.Logger) error {
	m := newMetrics(nil)
	if reg != nil {
		reg.MustRegister(m.logDropped, m.logTruncated)
	}
	type input struct {
		f    *os.File
		sink *sink
	}
	var ins []input
	for _, pair := range strings.Split(fds, ",") {
		fdText, name, ok := strings.Cut(pair, "=")
		fd, err := strconv.Atoi(fdText)
		if !ok || err != nil || name == "" {
			return fmt.Errorf("log: %s %q: not fd=name", LogFDsEnv, pair)
		}
		dst := &retryDest{target: target, name: name, m: m, logger: logger}
		if err := dst.open(); err != nil {
			// A destination that cannot open is a component whose lines are
			// dropped and counted until it opens, not a log process that
			// does not run.
			logger.Error("log: destination does not open; its lines are dropped until it does", "component", name, "err", err)
		}
		onDrop := func(c string, dropping bool, err error) {
			if dropping {
				logger.Error("log: a component's log is dropping lines", "component", c, "err", err)
				return
			}
			logger.Info("log: a component's log takes lines again", "component", c)
		}
		// Non-blocking before os.NewFile, so the reads can be given a
		// deadline at the stop: exec hands the descriptor over blocking.
		if err := syscall.SetNonblock(fd, true); err != nil {
			return fmt.Errorf("log: fd %d (%s): %w", fd, name, err)
		}
		ins = append(ins, input{f: os.NewFile(uintptr(fd), name), sink: newSink(name, dst, m, onDrop)})
	}
	if len(ins) == 0 {
		return errors.New("log: no pipe to read")
	}
	var wg sync.WaitGroup
	for _, in := range ins {
		wg.Add(1)
		go func() { defer wg.Done(); in.sink.feed(in.f) }()
	}
	for sig := range sigs {
		if sig == syscall.SIGHUP {
			for _, in := range ins {
				if err := in.sink.reopen(); err != nil {
					logger.Error("log: reopen", "component", in.sink.name, "err", err)
				}
			}
			continue
		}
		break
	}
	// What is already in the pipes is written; then every reader ends on its
	// deadline. A line still unfinished at the deadline was read from the
	// pipe and goes with this process: the one split line a hand-over is
	// allowed (#2715).
	deadline := time.Now().Add(stopDrain)
	for _, in := range ins {
		_ = in.f.SetReadDeadline(deadline)
	}
	wg.Wait()
	var errs []error
	for _, in := range ins {
		if err := in.sink.close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func openDest(t LogTarget, name string, m *metrics) (destination, error) {
	if t.Path == "syslog" {
		return openSyslogDest(t.Facility, "yarilo-"+name, m.logTruncated.WithLabelValues(name))
	}
	return openFileDest(t.Path, name)
}

// retryDest is a destination that comes back. One that could not open, as
// with a full disk, is tried again at every reopen and at most every few
// seconds on a write; a syslog socket that fails a write (syslogd
// restarted) is dialled again once and the line sent again.
type retryDest struct {
	target LogTarget
	name   string
	m      *metrics
	logger *slog.Logger

	mu    sync.Mutex
	dst   destination
	tried time.Time
}

const retryEvery = 5 * time.Second

func (d *retryDest) open() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.openLocked()
}

func (d *retryDest) openLocked() error {
	d.tried = time.Now()
	dst, err := openDest(d.target, d.name, d.m)
	if err != nil {
		return err
	}
	if d.dst != nil {
		_ = d.dst.close()
	}
	d.dst = dst
	return nil
}

func (d *retryDest) write(chunk []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dst == nil {
		if time.Since(d.tried) < retryEvery {
			return errNotOpen
		}
		if err := d.openLocked(); err != nil {
			return err
		}
		d.logger.Info("log: destination opens again", "component", d.name)
	}
	sd, isSyslog := d.dst.(*syslogDest)
	if !isSyslog {
		return d.dst.write(chunk)
	}
	sent, err := sd.writeFrom(chunk)
	if err != nil {
		// syslogd may have restarted under us: dial again and send once more
		// what did not go, never the lines that already went.
		if oerr := d.openLocked(); oerr == nil {
			if again, ok := d.dst.(*syslogDest); ok {
				_, err = again.writeFrom(chunk[sent:])
			}
		}
	}
	return err
}

func (d *retryDest) reopen() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dst == nil {
		return d.openLocked()
	}
	return d.dst.reopen()
}

func (d *retryDest) close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dst == nil {
		return nil
	}
	return d.dst.close()
}

var errNotOpen = errors.New("the destination is not open")
