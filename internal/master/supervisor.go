package master

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Timings of supervision. A component that has worked is started again at
// once; one that dies younger waits, doubling, and the wait returns to none
// once it has run WorkingUptime (the reference's rule, #2715).
type Timings struct {
	WorkingUptime time.Duration
	FirstDelay    time.Duration
	MaxDelay      time.Duration
	ReadyTimeout  time.Duration
}

// DefaultTimings are the production figures.
var DefaultTimings = Timings{
	WorkingUptime: 60 * time.Second,
	FirstDelay:    2 * time.Second,
	MaxDelay:      60 * time.Second,
	ReadyTimeout:  10 * time.Second,
}

// Options configure a Supervisor.
type Options struct {
	Logger     *slog.Logger
	Registerer prometheus.Registerer
	Timings    Timings
	// AsRoot sets each component's credential; false runs it as the
	// supervisor's own user, which only tests do.
	AsRoot bool
	// PreStart refuses a component the host is not set up for (the low-port
	// rules on FreeBSD). The component is then "refused" and the rest start.
	PreStart func(Component) error
	// IsOurs reports whether a recorded pid still names this component's
	// binary, so a survivor of a dead supervisor is stopped and a reused pid
	// is left alone.
	IsOurs func(pid int, c Component) bool
}

// State is what one component is doing.
type State string

const (
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateWaiting  State = "waiting"
	StateStopped  State = "stopped"
	StateRefused  State = "refused"
)

// Status is one component's line of status.
type Status struct {
	Name     string
	State    State
	PID      int
	Uptime   time.Duration
	Restarts int
	Why      string
}

// Supervisor runs a plan's components.
type Supervisor struct {
	plan  *Plan
	opts  Options
	log   *slog.Logger
	m     *metrics
	procs []*proc
	byNm  map[string]*proc
	lock  *os.File
	// pipes are every output the log process reads, the supervisor's own
	// first. The supervisor holds both ends for its life, so a log process
	// that dies costs no component an error on write (#2715).
	pipes []*outPipe
	logUp atomic.Bool // a log process is running and reading
	drain drainer

	ctx      context.Context
	cancel   context.CancelFunc
	runMu    sync.Mutex // orders addLoop against Stop's cancel
	wg       sync.WaitGroup
	stopOnce sync.Once
}

type proc struct {
	s   *Supervisor
	c   Component
	out *outPipe // its stdout and stderr; nil for the log process
	// quickFails counts the log process's young exits in a row: after three
	// it backs off like any other component.
	quickFails int

	mu        sync.Mutex
	state     State
	pid       int
	cmd       *exec.Cmd
	exited    chan struct{} // closed when the current process is gone
	started   time.Time
	restarts  int
	why       string
	down      bool // kept down by "stop" until "start"
	requested bool // the next exit is a requested restart
	delay     time.Duration
	kick      chan struct{}
	firstUp   chan struct{}
	firstOnce sync.Once
}

// New prepares a supervisor: the lock, the run directories, the logs.
// Nothing is started yet.
func New(plan *Plan, opts Options) (*Supervisor, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Timings == (Timings{}) {
		opts.Timings = DefaultTimings
	}
	s := &Supervisor{plan: plan, opts: opts, log: opts.Logger, m: newMetrics(opts.Registerer), byNm: map[string]*proc{}}
	// Made here, not in Run: a Stop that comes before Run must still stop it.
	s.ctx, s.cancel = context.WithCancel(context.Background())
	if err := s.prepareRunDir(); err != nil {
		return nil, err
	}
	lock, err := takeLock(filepath.Join(plan.RunDir, "master.lock"))
	if err != nil {
		return nil, err
	}
	s.lock = lock
	own, err := newOutPipe("master")
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	s.pipes = append(s.pipes, own)
	for _, c := range plan.Components {
		p := &proc{s: s, c: c, state: StateStopped, kick: make(chan struct{}, 1), firstUp: make(chan struct{})}
		if c.Name != LogComponent {
			if p.out, err = newOutPipe(c.Name); err != nil {
				_ = lock.Close()
				return nil, err
			}
			s.pipes = append(s.pipes, p.out)
		}
		s.procs = append(s.procs, p)
		s.byNm[c.Name] = p
	}
	s.drain.s = s
	return s, nil
}

func (s *Supervisor) prepareRunDir() error {
	if err := os.MkdirAll(s.plan.RunDir, 0o755); err != nil {
		return err
	}
	for _, d := range runDirs {
		path := filepath.Join(s.plan.RunDir, d.name)
		if err := os.MkdirAll(path, 0o750); err != nil {
			return err
		}
		if err := os.Chmod(path, 0o750); err != nil {
			return err
		}
		if s.opts.AsRoot {
			a := s.plan.Accounts[d.role]
			if err := os.Chown(path, int(a.UID), int(a.GID)); err != nil {
				return err
			}
		}
	}
	return nil
}

func takeLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another yarilo-master holds %s: %w", path, err)
	}
	return f, nil
}

// Run stops what a dead supervisor left running, starts the components in
// order, and supervises them until Stop.
func (s *Supervisor) Run() {
	s.stopSurvivors()
	for _, p := range s.procs {
		if !s.addLoop() {
			return
		}
		go p.loop()
		select {
		case <-p.firstUp:
		case <-s.ctx.Done():
			return
		}
	}
}

// addLoop counts one more component loop, unless the supervisor is stopping:
// Stop waits for every loop it counted, and none starts after it began.
func (s *Supervisor) addLoop() bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.ctx.Err() != nil {
		return false
	}
	s.wg.Add(1)
	return true
}

// stopSurvivors stops a component a previous supervisor left behind, found by
// its pid file, before anything starts: otherwise there would be two of it.
func (s *Supervisor) stopSurvivors() {
	for _, p := range s.procs {
		pid := readPid(p.pidFile())
		if pid <= 0 || syscall.Kill(pid, 0) != nil {
			_ = os.Remove(p.pidFile())
			continue
		}
		if s.opts.IsOurs == nil || !s.opts.IsOurs(pid, p.c) {
			_ = os.Remove(p.pidFile())
			continue
		}
		s.log.Warn("master: stopping a component a previous supervisor left running", "component", p.c.Name, "pid", pid)
		_ = syscall.Kill(pid, syscall.SIGTERM)
		if !waitGone(pid, s.plan.StopTimeout) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			waitGone(pid, 5*time.Second)
		}
		_ = os.Remove(p.pidFile())
	}
}

func waitGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func readPid(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func (p *proc) pidFile() string { return filepath.Join(p.s.plan.RunDir, p.c.Name+".pid") }

func (p *proc) set(f func()) {
	p.mu.Lock()
	f()
	p.mu.Unlock()
}

func (p *proc) markFirstUp() { p.firstOnce.Do(func() { close(p.firstUp) }) }

// loop runs one component for the supervisor's life.
func (p *proc) loop() {
	defer p.s.wg.Done()
	defer p.markFirstUp()
	for {
		if p.s.ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		down := p.down
		p.mu.Unlock()
		if down {
			p.set(func() { p.state, p.why = StateStopped, "stopped by request" })
			p.markFirstUp()
			if !p.waitKick(0, true) {
				return
			}
			continue
		}
		if p.s.opts.PreStart != nil {
			if err := p.s.opts.PreStart(p.c); err != nil {
				p.set(func() { p.state, p.why = StateRefused, err.Error() })
				p.s.log.Error("master: component refused", "component", p.c.Name, "err", err)
				p.markFirstUp()
				// A host setting, not a typo of ours: the rest start, and a
				// "start" from the operator tries again.
				if !p.waitKick(0, true) {
					return
				}
				continue
			}
		}
		if p.s.ctx.Err() != nil {
			return
		}
		p.drainKick() // a kick from before this start is not a reason to skip the next wait
		if p.c.Name == LogComponent {
			p.s.drain.stop() // never two readers on one pipe
		}
		ran, how, err := p.runOnce()
		if p.s.ctx.Err() != nil {
			return
		}
		p.drainKick() // a start asked for while it ran is no reason to skip the wait now
		if err != nil {
			how = "exit"
			p.s.log.Error("master: component could not be started", "component", p.c.Name, "err", err)
			p.markFirstUp() // one that cannot start does not hold the rest
		}
		p.mu.Lock()
		requested, down := p.requested, p.down
		p.requested = false
		delay := p.nextDelay(ran)
		if p.c.Name == LogComponent {
			delay = p.logDelay(ran)
		}
		if requested {
			delay = 0
			how = "requested"
		}
		if !down {
			p.restarts++
			p.s.m.restarts.WithLabelValues(p.c.Name, how).Inc()
		}
		p.state, p.delay = StateWaiting, delay
		p.mu.Unlock()
		if down {
			continue
		}
		if !requested {
			p.s.log.Warn("master: component exited", "component", p.c.Name, "how", how,
				"why", p.why, "uptime", ran.Round(time.Millisecond).String(), "next_start_in", delay.String())
		}
		if p.c.Name == LogComponent && delay > 0 {
			p.s.drain.start() // the components must not block on a full pipe
		}
		if !p.waitKick(delay, false) {
			return
		}
	}
}

// logDelay: a log process that had been working comes back at once, since
// every component's output waits on it; after three young exits in a row the
// usual backoff applies, so one that cannot start does not loop. Caller holds
// p.mu.
func (p *proc) logDelay(ran time.Duration) time.Duration {
	if ran >= p.s.opts.Timings.WorkingUptime {
		p.quickFails = 0
		return 0
	}
	p.quickFails++
	if p.quickFails < 3 {
		return 0
	}
	return p.nextDelay(ran)
}

// nextDelay: none for a component that has worked; doubling from FirstDelay
// for one that died young. Caller holds p.mu.
func (p *proc) nextDelay(ran time.Duration) time.Duration {
	t := p.s.opts.Timings
	if ran >= t.WorkingUptime {
		return 0
	}
	switch {
	case p.delay == 0:
		return t.FirstDelay
	case p.delay*2 > t.MaxDelay:
		return t.MaxDelay
	default:
		return p.delay * 2
	}
}

func (p *proc) drainKick() {
	select {
	case <-p.kick:
	default:
	}
}

// waitKick waits d, or for a kick; forever waits for a kick alone. False
// when the supervisor stops.
func (p *proc) waitKick(d time.Duration, forever bool) bool {
	var timer <-chan time.Time
	switch {
	case forever:
	case d <= 0:
		return p.s.ctx.Err() == nil
	default:
		t := time.NewTimer(d)
		defer t.Stop()
		timer = t.C
	}
	select {
	case <-p.s.ctx.Done():
		return false
	case <-p.kick:
		return true
	case <-timer:
		return true
	}
}

// runOnce starts the component and returns when its process is gone.
func (p *proc) runOnce() (time.Duration, string, error) {
	for _, sock := range p.c.Ready {
		_ = os.Remove(sock) // a dead process's socket must not read as ready
	}
	cmd := exec.Command(p.c.Bin, p.c.Args...) //nolint:gosec,noctx // the binary is a checked, root-owned path from the plan; the supervisor stops it itself
	cmd.Env = p.c.Env
	cmd.Dir = "/"
	if p.c.Name == LogComponent {
		// Every read end, in order, from fd 3; the log process's own lines go
		// where the supervisor's fallback goes.
		fds := make([]string, 0, len(p.s.pipes))
		for i, op := range p.s.pipes {
			f, err := dup(op.r, false)
			if err != nil {
				closeAll(cmd.ExtraFiles)
				return 0, "", err
			}
			cmd.ExtraFiles = append(cmd.ExtraFiles, f)
			fds = append(fds, fmt.Sprintf("%d=%s", 3+i, op.name))
		}
		defer closeAll(cmd.ExtraFiles) // the child has its own; these copies were for exec
		cmd.Env = append(append([]string{}, cmd.Env...), LogFDsEnv+"="+strings.Join(fds, ","))
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	} else {
		cmd.Stdout, cmd.Stderr = p.out.w, p.out.w
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	setDeathSignal(cmd.SysProcAttr)
	if p.s.opts.AsRoot {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: p.c.Account.UID, Gid: p.c.Account.GID, Groups: p.c.Account.Groups}
	}
	if err := cmd.Start(); err != nil {
		return 0, "", err
	}
	if p.c.Name == LogComponent {
		p.s.logUp.Store(true)
	}

	started := time.Now()
	exited := make(chan struct{})
	pid := cmd.Process.Pid
	p.set(func() {
		p.cmd, p.pid, p.started, p.exited, p.why = cmd, pid, started, exited, ""
		p.state = StateStarting
	})
	// Stop may have looked for this process before it existed, while the
	// pre-start check ran; then it is stopped here, and Stop waits for it.
	if p.s.ctx.Err() != nil {
		go p.terminate()
	}
	_ = os.WriteFile(p.pidFile(), []byte(strconv.Itoa(pid)+"\n"), 0o644)
	p.s.m.up.WithLabelValues(p.c.Name).Set(1)
	p.s.m.started.WithLabelValues(p.c.Name).Set(float64(started.Unix()))

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	if p.waitReady(waited) {
		p.set(func() { p.state = StateRunning })
	}
	p.markFirstUp()

	werr := <-waited
	ran := time.Since(started)
	if p.c.Name == LogComponent {
		p.s.logUp.Store(false)
	}
	close(exited)
	_ = os.Remove(p.pidFile())
	p.s.m.up.WithLabelValues(p.c.Name).Set(0)
	how, why := exitReason(werr)
	p.set(func() { p.cmd, p.pid, p.why = nil, 0, why })
	return ran, how, nil
}

func closeAll(fs []*os.File) {
	for _, f := range fs {
		_ = f.Close()
	}
}

// waitReady waits until every ready socket accepts a connection, the process
// exits, or the timeout passes. A component with no ready sockets is ready at
// once.
func (p *proc) waitReady(waited chan error) bool {
	if len(p.c.Ready) == 0 {
		return true
	}
	deadline := time.Now().Add(p.s.opts.Timings.ReadyTimeout)
	for _, sock := range p.c.Ready {
		for {
			if d, err := (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext(p.s.ctx, "unix", sock); err == nil {
				_ = d.Close()
				break
			}
			select {
			case err := <-waited:
				waited <- err // runOnce reads it again
				return false
			case <-p.s.ctx.Done():
				return false
			case <-time.After(50 * time.Millisecond):
			}
			if time.Now().After(deadline) {
				p.s.log.Warn("master: component not ready in time; starting the next", "component", p.c.Name, "socket", sock)
				return false
			}
		}
	}
	return true
}

func exitReason(err error) (how, why string) {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			sig := ws.Signal()
			return "signal", fmt.Sprintf("killed by signal %d (%s)", int(sig), sig)
		}
		return "exit", fmt.Sprintf("exited with status %d", ee.ExitCode())
	}
	if err != nil {
		return "exit", err.Error()
	}
	return "exit", "exited with status 0"
}

// terminate stops the current process: SIGTERM, then SIGKILL after the stop
// timeout. It returns once the process is gone.
func (p *proc) terminate() {
	p.mu.Lock()
	cmd, exited := p.cmd, p.exited
	p.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
		return
	case <-time.After(p.s.plan.StopTimeout):
	}
	p.s.log.Warn("master: component did not stop in time; killing it", "component", p.c.Name, "pid", cmd.Process.Pid)
	_ = cmd.Process.Kill()
	<-exited
}

// Stop stops every component in reverse order and closes the logs.
func (s *Supervisor) Stop() {
	s.stopOnce.Do(func() {
		s.runMu.Lock()
		s.cancel()
		s.runMu.Unlock()
		for i := len(s.procs) - 1; i >= 0; i-- {
			s.procs[i].terminate()
		}
		s.wg.Wait()
		s.drain.stop()
		if s.lock != nil {
			_ = s.lock.Close()
		}
	})
}

func (s *Supervisor) find(names []string) ([]*proc, error) {
	if len(names) == 0 {
		return nil, errors.New("name at least one component")
	}
	out := make([]*proc, 0, len(names))
	for _, n := range names {
		p, ok := s.byNm[n]
		if !ok {
			return nil, fmt.Errorf("no component %q", n)
		}
		out = append(out, p)
	}
	return out, nil
}

// StopComponents stops the named components and keeps them down until
// StartComponents.
func (s *Supervisor) StopComponents(names ...string) error {
	ps, err := s.find(names)
	if err != nil {
		return err
	}
	for _, p := range ps {
		p.set(func() { p.down = true })
		p.terminate()
	}
	return nil
}

// StartComponents starts the named components now.
func (s *Supervisor) StartComponents(names ...string) error {
	ps, err := s.find(names)
	if err != nil {
		return err
	}
	for _, p := range ps {
		p.set(func() { p.down = false; p.delay = 0 })
		select {
		case p.kick <- struct{}{}:
		default:
		}
	}
	return nil
}

// Restart stops the named components and starts them again with no delay.
func (s *Supervisor) Restart(names ...string) error {
	ps, err := s.find(names)
	if err != nil {
		return err
	}
	for _, p := range ps {
		p.mu.Lock()
		running := p.cmd != nil
		p.requested = running
		p.down = false
		p.mu.Unlock()
		if running {
			p.terminate()
		}
		select {
		case p.kick <- struct{}{}:
		default:
		}
	}
	return nil
}

// OwnLog is where the supervisor's own lines go: its own pipe, which the log
// process reads as the component "master". The supervisor never waits on it:
// the pipe's end is written without blocking, and a line that does not fit,
// or any line while no log process runs, goes to fallback (stderr, which the
// port's rc script sends to syslog).
func (s *Supervisor) OwnLog(fallback io.Writer) io.Writer {
	return &ownWriter{s: s, pipe: s.pipes[0].w, fallback: fallback}
}

// Reopen has the log process reopen every file.
func (s *Supervisor) Reopen() error {
	p := s.byNm[LogComponent]
	if p == nil {
		return nil
	}
	p.mu.Lock()
	cmd := p.cmd
	p.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return errors.New("no log process runs to reopen the files")
	}
	return cmd.Process.Signal(syscall.SIGHUP)
}

// Status is every component's state, in start order.
func (s *Supervisor) Status() []Status {
	out := make([]Status, 0, len(s.procs))
	for _, p := range s.procs {
		p.mu.Lock()
		st := Status{Name: p.c.Name, State: p.state, PID: p.pid, Restarts: p.restarts, Why: p.why}
		if p.pid != 0 {
			st.Uptime = time.Since(p.started)
		}
		if p.state == StateWaiting && p.delay > 0 {
			st.Why = strings.TrimSpace(st.Why + "; next start in " + p.delay.String())
		}
		p.mu.Unlock()
		out = append(out, st)
	}
	return out
}
