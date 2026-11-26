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
		dst, err := openDest(target, name, m)
		if err != nil {
			// A destination that cannot open is a component whose lines are
			// dropped and counted, not a log process that does not run.
			logger.Error("log: destination does not open; its lines are dropped", "component", name, "err", err)
			dst = refusedDest{err: err}
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

// refusedDest stands for a destination that could not open.
type refusedDest struct{ err error }

func (d refusedDest) write([]byte) error { return d.err }
func (d refusedDest) reopen() error      { return d.err }
func (d refusedDest) close() error       { return nil }
