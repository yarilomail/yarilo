package master

import (
	"bufio"
	"bytes"
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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

var helperBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "master-helper")
	if err != nil {
		panic(err)
	}
	helperBin = filepath.Join(dir, "helper")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	out, err := exec.CommandContext(ctx, "go", "build", "-o", helperBin, "./testdata/helper").CombinedOutput()
	cancel()
	if err != nil {
		panic(fmt.Sprintf("build helper: %v\n%s", err, out))
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// fast are timings that keep a test short; the rule they stand for is the
// same as production's.
var fast = Timings{WorkingUptime: 400 * time.Millisecond, FirstDelay: 100 * time.Millisecond, MaxDelay: 400 * time.Millisecond, ReadyTimeout: 3 * time.Second}

// shortDir is a directory whose sockets fit the unix path limit on macOS.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "m")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

type rig struct {
	t    *testing.T
	dir  string
	plan *Plan
	reg  *prometheus.Registry
	sup  *Supervisor
}

// comp is a component run by the helper, with extra environment.
func (r *rig) comp(name string, env ...string) Component {
	c := Component{Name: name, Bin: helperBin, Env: append([]string{"HELPER_STARTS=" + r.starts(name)}, env...)}
	return c
}

func (r *rig) starts(name string) string { return filepath.Join(r.dir, name+".starts") }

func newRig(t *testing.T, comps func(r *rig) []Component, mutate ...func(*Options)) *rig {
	t.Helper()
	dir := shortDir(t)
	r := &rig{t: t, dir: dir, reg: prometheus.NewRegistry()}
	logs := filepath.Join(dir, "log")
	if err := os.Mkdir(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	r.plan = &Plan{RunDir: filepath.Join(dir, "run"), LogPath: logs, StopTimeout: 2 * time.Second, Accounts: map[role]Account{}}
	r.plan.Components = comps(r)
	opts := Options{Logger: slog.New(slog.DiscardHandler), Registerer: r.reg, Timings: fast}
	for _, f := range mutate {
		f(&opts)
	}
	sup, err := New(r.plan, opts)
	if err != nil {
		t.Fatal(err)
	}
	r.sup = sup
	t.Cleanup(sup.Stop)
	return r
}

func (r *rig) startTimes(name string) []time.Time {
	b, _ := os.ReadFile(r.starts(name))
	var out []time.Time
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if n, err := strconv.ParseInt(strings.TrimPrefix(l, "start "), 10, 64); err == nil {
			out = append(out, time.Unix(0, n))
		}
	}
	return out
}

func (r *rig) waitStarts(name string, n int, within time.Duration) []time.Time {
	r.t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if ts := r.startTimes(name); len(ts) >= n {
			return ts
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.t.Fatalf("%s started %d times in %v, want %d", name, len(r.startTimes(name)), within, n)
	return nil
}

func (r *rig) status(name string) Status {
	for _, st := range r.sup.Status() {
		if st.Name == name {
			return st
		}
	}
	r.t.Fatalf("no status for %s", name)
	return Status{}
}

func (r *rig) waitState(name string, want State, within time.Duration) Status {
	r.t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if st := r.status(name); st.State == want && (want != StateRunning || st.PID > 0) {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.t.Fatalf("%s is %+v, want %s", name, r.status(name), want)
	return Status{}
}

// Each component waits for the one before it to answer on its socket, and a
// socket file a dead process left behind does not count as an answer.
func TestComponentsStartInOrderAndWaitForAnAnsweringSocket(t *testing.T) {
	var sock string
	r := newRig(t, func(r *rig) []Component {
		sock = filepath.Join(r.dir, "a.sock")
		// A stale socket file, as a dead process leaves it.
		ln, err := new(net.ListenConfig).Listen(t.Context(), "unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		if ul, ok := ln.(*net.UnixListener); ok {
			ul.SetUnlinkOnClose(false)
		}
		_ = ln.Close()
		a := r.comp("locks", "HELPER_LISTEN="+sock, "HELPER_DELAY_MS=300")
		a.Ready = []string{sock}
		return []Component{a, r.comp("imap")}
	})
	began := time.Now()
	go r.sup.Run()
	a := r.waitStarts("locks", 1, 3*time.Second)[0]
	b := r.waitStarts("imap", 1, 3*time.Second)[0]
	if gap := b.Sub(a); gap < 300*time.Millisecond {
		t.Errorf("imap started %v after locks, before locks served its socket (300 ms); the stale file was taken for ready", gap)
	}
	t.Logf("locks at +%v, imap at +%v", a.Sub(began), b.Sub(began))

	// Stop runs in reverse: imap goes first, so locks is still serving then.
	r.sup.Stop()
	for _, name := range []string{"locks", "imap"} {
		if st := r.status(name); st.PID != 0 {
			t.Errorf("%s still has pid %d after Stop", name, st.PID)
		}
	}
}

// A component that has worked is started again at once; one that dies young
// waits, doubling to the ceiling.
func TestAWorkingComponentRestartsAtOnceAndAYoungOneBacksOff(t *testing.T) {
	r := newRig(t, func(r *rig) []Component {
		return []Component{r.comp("imap"), r.comp("pop3", "HELPER_MODE=crash")}
	})
	go r.sup.Run()
	st := r.waitState("imap", StateRunning, 3*time.Second)
	time.Sleep(fast.WorkingUptime + 100*time.Millisecond)
	killed := time.Now()
	if err := syscall.Kill(st.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	ts := r.waitStarts("imap", 2, 3*time.Second)
	if back := ts[1].Sub(killed); back > 150*time.Millisecond {
		t.Errorf("imap, which had worked, came back %v after the kill; want at once", back)
	}
	if why := r.status("imap").Why; why != "" && why != "killed by signal 9 (killed)" {
		t.Errorf("the kill reads %q", why)
	}
	if got := testutil.ToFloat64(r.sup.m.restarts.WithLabelValues("imap", "signal")); got != 1 {
		t.Errorf("restarts{imap,signal} = %v, want 1", got)
	}

	crashes := r.waitStarts("pop3", 5, 5*time.Second)
	var gaps []time.Duration
	for i := 1; i < len(crashes); i++ {
		gaps = append(gaps, crashes[i].Sub(crashes[i-1]).Round(10*time.Millisecond))
	}
	t.Logf("gaps between crash restarts: %v", gaps)
	want := []time.Duration{100, 200, 400, 400}
	for i, w := range want {
		w *= time.Millisecond
		if gaps[i] < w-30*time.Millisecond || gaps[i] > w+150*time.Millisecond {
			t.Errorf("gap %d = %v, want about %v", i, gaps[i], w)
		}
	}
	if got := testutil.ToFloat64(r.sup.m.restarts.WithLabelValues("pop3", "exit")); got < 4 {
		t.Errorf("restarts{pop3,exit} = %v, want at least 4", got)
	}
}

// A requested restart has no delay and is counted apart; nothing restarts
// while the supervisor stops.
func TestARequestedRestartIsImmediateAndAStopRestartsNothing(t *testing.T) {
	r := newRig(t, func(r *rig) []Component { return []Component{r.comp("imap")} })
	go r.sup.Run()
	r.waitStarts("imap", 1, 3*time.Second)
	r.waitState("imap", StateRunning, 3*time.Second)
	asked := time.Now()
	if err := r.sup.Restart("imap"); err != nil {
		t.Fatal(err)
	}
	ts := r.waitStarts("imap", 2, 3*time.Second)
	if d := ts[1].Sub(asked); d > 2*time.Second {
		t.Errorf("the requested restart took %v", d)
	}
	if got := testutil.ToFloat64(r.sup.m.restarts.WithLabelValues("imap", "requested")); got != 1 {
		t.Errorf("restarts{imap,requested} = %v, want 1", got)
	}
	r.waitState("imap", StateRunning, 3*time.Second)
	r.sup.Stop()
	time.Sleep(fast.FirstDelay * 3)
	if n := len(r.startTimes("imap")); n != 2 {
		t.Errorf("imap started %d times; a stop restarted it", n)
	}
}

// A component that ignores SIGTERM is killed after the stop timeout.
func TestAComponentThatIgnoresTermIsKilledAfterTheTimeout(t *testing.T) {
	r := newRig(t, func(r *rig) []Component { return []Component{r.comp("imap", "HELPER_MODE=ignore-term")} })
	r.plan.StopTimeout = 300 * time.Millisecond
	go r.sup.Run()
	r.waitStarts("imap", 1, 3*time.Second)
	st := r.waitState("imap", StateRunning, 3*time.Second)
	began := time.Now()
	r.sup.Stop()
	took := time.Since(began)
	if took < 300*time.Millisecond {
		t.Errorf("Stop took %v; it did not wait the stop timeout", took)
	}
	if syscall.Kill(st.PID, 0) == nil {
		t.Errorf("pid %d outlived the stop", st.PID)
	}
}

// stop keeps a component down across its own exit; start brings it back.
func TestStopKeepsAComponentDownAndStartBringsItBack(t *testing.T) {
	r := newRig(t, func(r *rig) []Component { return []Component{r.comp("imap")} })
	go r.sup.Run()
	// Running is set at exec; the helper's own start line comes after it.
	r.waitStarts("imap", 1, 3*time.Second)
	r.waitState("imap", StateRunning, 3*time.Second)
	if err := r.sup.StopComponents("imap"); err != nil {
		t.Fatal(err)
	}
	r.waitState("imap", StateStopped, 3*time.Second)
	time.Sleep(fast.MaxDelay + 100*time.Millisecond)
	if n := len(r.startTimes("imap")); n != 1 {
		t.Fatalf("imap started %d times while stopped", n)
	}
	if err := r.sup.StartComponents("imap"); err != nil {
		t.Fatal(err)
	}
	r.waitStarts("imap", 2, 3*time.Second)
	r.waitState("imap", StateRunning, 3*time.Second)
	if err := r.sup.StopComponents("nope"); err == nil {
		t.Error("stopping an unknown component was accepted")
	}
}

// The control socket answers status in fixed fields, carries the commands,
// and refuses a peer that is not the allowed uid.
func TestTheControlSocket(t *testing.T) {
	r := newRig(t, func(r *rig) []Component { return []Component{r.comp("imap"), r.comp("pop3")} })
	go r.sup.Run()
	r.waitState("pop3", StateRunning, 3*time.Second)
	sock := filepath.Join(r.dir, "c.sock")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	uid := uint32(os.Getuid())
	go func() { _ = r.sup.Serve(ctx, sock, uid) }()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("control socket mode %v, want 0600", fi.Mode().Perm())
	}
	var out bytes.Buffer
	if err := Ctl(ctx, sock, []string{"status"}, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("status gave %d lines: %q", len(lines), out.String())
	}
	for _, l := range lines {
		f := strings.Split(l, "\t")
		if len(f) != 6 || f[1] != string(StateRunning) || f[2] == "-" {
			t.Errorf("status line %q, want six fields of a running component", l)
		}
	}
	if err := Ctl(ctx, sock, []string{"stop", "pop3"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	r.waitState("pop3", StateStopped, 3*time.Second)
	// status of named components answers for those alone, and fails when one
	// of them is not running.
	out.Reset()
	if err := Ctl(ctx, sock, []string{"status", "imap"}, &out); err != nil || !strings.HasPrefix(out.String(), "imap\t") || strings.Count(out.String(), "\n") != 1 {
		t.Errorf("status imap = %q, %v; want one running line and success", out.String(), err)
	}
	out.Reset()
	if err := Ctl(ctx, sock, []string{"status", "pop3"}, &out); err == nil || !strings.Contains(err.Error(), "not running: pop3") || !strings.HasPrefix(out.String(), "pop3\tstopped") {
		t.Errorf("status pop3 while stopped = %q, %v; want its line and a failure", out.String(), err)
	}
	if err := Ctl(ctx, sock, []string{"status", "nope"}, io.Discard); err == nil {
		t.Error("status of an unknown component succeeded")
	}
	if err := Ctl(ctx, sock, []string{"start", "pop3"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	r.waitState("pop3", StateRunning, 3*time.Second)
	if err := Ctl(ctx, sock, []string{"restart", "nope"}, io.Discard); err == nil {
		t.Error("restart of an unknown component was accepted")
	}

	other := filepath.Join(r.dir, "o.sock")
	go func() { _ = r.sup.Serve(ctx, other, uid+1) }()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(other); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := Ctl(ctx, other, []string{"status"}, io.Discard); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("a peer of another uid got %v, want not allowed", err)
	}
}

// A second supervisor beside a live one is refused.
func TestASecondSupervisorIsRefused(t *testing.T) {
	r := newRig(t, func(r *rig) []Component { return []Component{r.comp("imap")} })
	if _, err := New(r.plan, Options{Logger: slog.New(slog.DiscardHandler), Timings: fast}); err == nil {
		t.Fatal("a second supervisor on the same run directory was allowed")
	}
}

// A component a dead supervisor left running is stopped before anything
// starts, so there are never two of it.
func TestASurvivorOfAPreviousSupervisorIsStoppedFirst(t *testing.T) {
	survivor := exec.Command(helperBin) //nolint:noctx // stopped by the supervisor under test
	if err := survivor.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = survivor.Wait(); close(done) }()
	t.Cleanup(func() { _ = survivor.Process.Kill() })
	r := newRig(t, func(r *rig) []Component {
		if err := os.MkdirAll(filepath.Join(r.dir, "run"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.dir, "run", "imap.pid"), []byte(strconv.Itoa(survivor.Process.Pid)), 0o644); err != nil {
			t.Fatal(err)
		}
		return []Component{r.comp("imap")}
	}, func(o *Options) { o.IsOurs = func(pid int, _ Component) bool { return pid == survivor.Process.Pid } })
	go r.sup.Run()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the survivor was not stopped")
	}
	ts := r.waitStarts("imap", 1, 3*time.Second)
	if len(ts) != 1 {
		t.Errorf("imap started %d times", len(ts))
	}
}

// SIGHUP's reopen: after newsyslog renames the file, new lines land in a new
// one.
func TestReopenWritesToANewFileAfterARename(t *testing.T) {
	r := newRig(t, func(r *rig) []Component {
		return []Component{r.comp("imap", "HELPER_SAY=hello", "HELPER_SAY_MS=50")}
	})
	go r.sup.Run()
	logFile := filepath.Join(r.plan.LogPath, "imap.log")
	waitFor(t, func() bool { b, _ := os.ReadFile(logFile); return bytes.Contains(b, []byte("hello")) })
	if err := os.Rename(logFile, logFile+".0"); err != nil {
		t.Fatal(err)
	}
	if err := r.sup.Reopen(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { b, _ := os.ReadFile(logFile); return bytes.Contains(b, []byte("hello")) })
}

// A destination that refuses every write does not stop the component: its
// lines are drained and counted.
func TestALogThatRefusesWritesDoesNotStopTheComponent(t *testing.T) {
	m := newMetrics(nil)
	dst := &refusing{}
	var drops atomic.Int32
	s := newSink("imap", dst, m, func(string, bool, error) { drops.Add(1) })
	pr, pw := io.Pipe()
	fed := make(chan struct{})
	go func() { defer close(fed); s.feed(pr) }()
	wrote := make(chan struct{})
	go func() {
		defer close(wrote)
		for i := 0; i < 3*queued; i++ {
			if _, err := fmt.Fprintf(pw, "line %d\n", i); err != nil {
				return
			}
		}
	}()
	select {
	case <-wrote:
	case <-time.After(10 * time.Second):
		t.Fatal("the writer blocked on a log that refuses writes")
	}
	_ = pw.Close()
	<-fed // as the supervisor does: the reader is done before the sink closes
	_ = s.close()
	if got := testutil.ToFloat64(m.logDropped.WithLabelValues("imap")); got != float64(3*queued) {
		t.Errorf("dropped = %v, want %d", got, 3*queued)
	}
	if drops.Load() != 1 {
		t.Errorf("the drop was reported %d times, want once while it lasted", drops.Load())
	}
}

type refusing struct{}

func (refusing) write([]byte) error { return errors.New("no space left on device") }
func (refusing) reopen() error      { return nil }
func (refusing) close() error       { return nil }

func TestALongSyslogLineIsCutAndMarked(t *testing.T) {
	msg := strings.Repeat("é", 100) // two bytes each
	got, cut := fitSyslog(msg, 61)
	if !cut || len(got) > 61 || !strings.HasSuffix(got, "bytes]") || !utfValid(got) {
		t.Errorf("fitSyslog = %q (%d bytes), cut %v", got, len(got), cut)
	}
	if got, cut := fitSyslog("short", 61); cut || got != "short" {
		t.Errorf("a short line was changed: %q", got)
	}
}

func utfValid(s string) bool { return strings.ToValidUTF8(s, "?") == s }

func TestTheLevelOfAJSONLineIsItsSyslogPriority(t *testing.T) {
	for line, want := range map[string]string{
		`{"time":"x","level":"ERROR","msg":"m"}`: "ERROR",
		`{"level":"WARN+2","msg":"m"}`:           "WARN",
		`{"level":"debug"}`:                      "DEBUG",
		`not json`:                               "INFO",
	} {
		if got := levelOf(line); got != want {
			t.Errorf("levelOf(%q) = %s, want %s", line, got, want)
		}
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in 5 s")
}

// --- the plan ---

type planRig struct {
	dir, libexec, envDir, logs string
	cfg                        *config.Config
	chk                        Checker
}

func newPlanRig(t *testing.T, comps ...string) *planRig {
	t.Helper()
	dir := shortDir(t)
	p := &planRig{dir: dir, libexec: filepath.Join(dir, "libexec"), envDir: filepath.Join(dir, "etc"), logs: filepath.Join(dir, "log")}
	for _, d := range []string{p.libexec, p.envDir, p.logs} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range comps {
		if err := os.WriteFile(filepath.Join(p.libexec, "yarilo-"+c), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Defaults()
	cfg.Master.Components = comps
	cfg.Master.LibexecDir = p.libexec
	cfg.Master.EnvDir = p.envDir
	cfg.Master.LogPath = p.logs
	cfg.Master.RunDir = filepath.Join(dir, "run")
	cfg.Log.Level = "info"
	p.cfg = cfg
	me := uint32(os.Getuid())
	p.chk = Checker{
		TrustedUID: me,
		Lookup:     func(name string) (Account, error) { return Account{Name: name, UID: me + 1000, GID: me + 1000}, nil },
		BaseEnv:    []string{"PATH=/usr/bin:/bin"},
	}
	return p
}

func envOf(p *Plan, name string) map[string]string {
	out := map[string]string{}
	for _, c := range p.Components {
		if c.Name != name {
			continue
		}
		for _, kv := range c.Env {
			k, v, _ := strings.Cut(kv, "=")
			out[k] = v
		}
	}
	return out
}

func TestAnUnknownOrMissingComponentStartsNothing(t *testing.T) {
	p := newPlanRig(t, "imap")
	p.cfg.Master.Components = []string{"imap", "imapp", "pop3"}
	_, err := p.chk.Build(p.cfg, "")
	if err == nil {
		t.Fatal("a list with a typo and a missing binary was accepted")
	}
	for _, want := range []string{`unknown component "imapp"`, "yarilo-pop3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s: %v", want, err)
		}
	}
}

func TestNoComponentRunsAsRoot(t *testing.T) {
	p := newPlanRig(t, "imap")
	p.chk.Lookup = func(name string) (Account, error) {
		if name == "yarilo-login" {
			return Account{Name: name, UID: 0, GID: 0}, nil
		}
		return Account{Name: name, UID: 5000, GID: 5000}, nil
	}
	_, err := p.chk.Build(p.cfg, "")
	if err == nil || !strings.Contains(err.Error(), "master_login_user") {
		t.Fatalf("a root login user was accepted: %v", err)
	}
}

// A component gets the plan's environment and nothing of the supervisor's;
// a password file reaches its own component alone.
func TestAComponentGetsItsOwnEnvironmentOnly(t *testing.T) {
	t.Setenv("LEAKED_FROM_ROOT", "1")
	p := newPlanRig(t, "auth", "imap")
	p.cfg.Master.Environment = map[string]string{"GOMAXPROCS": "2"}
	if err := os.WriteFile(filepath.Join(p.envDir, "auth.env"), []byte("# db\nPGPASSWORD='s3cret'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := p.chk.Build(p.cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	auth, imap := envOf(plan, "auth"), envOf(plan, "imap")
	if auth["PGPASSWORD"] != "s3cret" {
		t.Errorf("auth did not get its password: %v", auth)
	}
	if _, ok := imap["PGPASSWORD"]; ok {
		t.Error("auth's password reached imap")
	}
	for name, env := range map[string]map[string]string{"auth": auth, "imap": imap} {
		if env["GOMAXPROCS"] != "2" {
			t.Errorf("%s lacks master_environment", name)
		}
		if _, ok := env["LEAKED_FROM_ROOT"]; ok {
			t.Errorf("the supervisor's environment reached %s", name)
		}
		if env["TELEMETRY_LISTEN"] == "" || env["LOG_LEVEL"] != "info" || env["PATH"] == "" {
			t.Errorf("%s lacks its fixed variables: %v", name, env)
		}
	}
	if imap["TELEMETRY_LISTEN"] != "127.0.0.1:18085" {
		t.Errorf("imap telemetry %s, want the rc script's 18085", imap["TELEMETRY_LISTEN"])
	}

	// And on the process itself: run it under a supervisor and read back.
	envFile := filepath.Join(shortDir(t), "env")
	r := newRig(t, func(r *rig) []Component {
		c := r.comp("imap", "HELPER_ENV="+envFile)
		c.Env = append(c.Env, plan.Components[1].Env...)
		return []Component{c}
	})
	go r.sup.Run()
	waitFor(t, func() bool { _, err := os.Stat(envFile); return err == nil })
	b, _ := os.ReadFile(envFile)
	if bytes.Contains(b, []byte("LEAKED_FROM_ROOT")) {
		t.Error("the running component sees the supervisor's environment")
	}
	if !bytes.Contains(b, []byte("GOMAXPROCS=2")) {
		t.Error("the running component lacks GOMAXPROCS")
	}
}

func TestFilesTheSupervisorTrustsMustBeTheTrustedUsersAndClosed(t *testing.T) {
	t.Run("a group-writable libexec", func(t *testing.T) {
		p := newPlanRig(t, "imap")
		if err := os.Chmod(p.libexec, 0o775); err != nil {
			t.Fatal(err)
		}
		if _, err := p.chk.Build(p.cfg, ""); err == nil || !strings.Contains(err.Error(), "writable") {
			t.Errorf("accepted: %v", err)
		}
	})
	t.Run("a symlinked binary", func(t *testing.T) {
		p := newPlanRig(t)
		if err := os.Symlink(helperBin, filepath.Join(p.libexec, "yarilo-imap")); err != nil {
			t.Fatal(err)
		}
		p.cfg.Master.Components = []string{"imap"}
		if _, err := p.chk.Build(p.cfg, ""); err == nil {
			t.Error("a symlink in libexec was accepted")
		}
	})
	t.Run("a readable password file", func(t *testing.T) {
		p := newPlanRig(t, "dict")
		if err := os.WriteFile(filepath.Join(p.envDir, "dict.env"), []byte("A=b\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := p.chk.Build(p.cfg, ""); err == nil || !strings.Contains(err.Error(), "0600") {
			t.Errorf("accepted: %v", err)
		}
	})
	t.Run("a log path that is neither syslog nor a directory", func(t *testing.T) {
		p := newPlanRig(t, "imap")
		p.cfg.Master.LogPath = filepath.Join(p.dir, "nowhere")
		if _, err := p.chk.Build(p.cfg, ""); err == nil || !strings.Contains(err.Error(), "master_log_path") {
			t.Errorf("accepted: %v", err)
		}
	})
}

// A crash restart is logged as a WARN with what the operator needs.
func TestAnExitIsAWarningNamingTheComponent(t *testing.T) {
	var buf safeBuffer
	r := newRig(t, func(r *rig) []Component { return []Component{r.comp("pop3", "HELPER_MODE=crash")} },
		func(o *Options) { o.Logger = slog.New(slog.NewJSONHandler(&buf, nil)) })
	go r.sup.Run()
	r.waitStarts("pop3", 2, 3*time.Second)
	sc := bufio.NewScanner(strings.NewReader(buf.String()))
	for sc.Scan() {
		l := sc.Text()
		if strings.Contains(l, "component exited") {
			for _, want := range []string{`"level":"WARN"`, `"component":"pop3"`, `exited with status 3`, `"next_start_in"`} {
				if !strings.Contains(l, want) {
					t.Errorf("the exit line lacks %s: %s", want, l)
				}
			}
			return
		}
	}
	t.Fatalf("no exit line in %s", buf.String())
}

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *safeBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buf.String() }

// A Stop that comes before Run stops it: nothing starts after.
func TestAStopBeforeRunStartsNothing(t *testing.T) {
	r := newRig(t, func(r *rig) []Component { return []Component{r.comp("imap")} })
	r.sup.Stop()
	done := make(chan struct{})
	go func() { r.sup.Run(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run after Stop did not return")
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(r.startTimes("imap")); n != 0 {
		t.Errorf("imap started %d times after the stop", n)
	}
}

// A component that is in its pre-start check when the stop comes is not left
// running, and the stop ends.
func TestAStopDuringThePreStartCheckLeavesNothingRunning(t *testing.T) {
	inCheck, release := make(chan struct{}), make(chan struct{})
	r := newRig(t, func(r *rig) []Component { return []Component{r.comp("imap")} },
		func(o *Options) {
			o.PreStart = func(Component) error { close(inCheck); <-release; return nil }
		})
	go r.sup.Run()
	<-inCheck
	stopped := make(chan struct{})
	go func() { r.sup.Stop(); close(stopped) }()
	time.Sleep(100 * time.Millisecond) // Stop has looked for a process and found none
	close(release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the stop never ended")
	}
	if st := r.status("imap"); st.PID != 0 {
		t.Errorf("imap left running as pid %d", st.PID)
	}
}

// A start or restart leaves no kick behind that would cut the next wait
// after a crash short.
func TestAKickDoesNotOutliveTheStartItAskedFor(t *testing.T) {
	r := newRig(t, func(r *rig) []Component { return []Component{r.comp("imap")} })
	go r.sup.Run()
	r.waitStarts("imap", 1, 3*time.Second)
	st := r.waitState("imap", StateRunning, 3*time.Second)
	if err := r.sup.StartComponents("imap"); err != nil { // running already: the kick is left over
		t.Fatal(err)
	}
	killed := time.Now()
	if err := syscall.Kill(st.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	ts := r.waitStarts("imap", 2, 3*time.Second)
	if d := ts[1].Sub(killed); d < fast.FirstDelay-20*time.Millisecond {
		t.Errorf("a young crash came back after %v; a leftover kick skipped the %v wait", d, fast.FirstDelay)
	}
}

func TestAWritableRunDirectoryStopsTheStart(t *testing.T) {
	p := newPlanRig(t, "imap")
	if err := os.Mkdir(p.cfg.Master.RunDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p.cfg.Master.RunDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := p.chk.Build(p.cfg, ""); err == nil || !strings.Contains(err.Error(), "master_run_dir") {
		t.Errorf("a world-writable run directory was accepted: %v", err)
	}
}

func TestAKilledComponentIsNamedByItsSignal(t *testing.T) {
	cmd := exec.Command(helperBin) //nolint:noctx // killed right here
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Process.Kill()
	how, why := exitReason(cmd.Wait())
	if how != "signal" || why != "killed by signal 9 (killed)" {
		t.Errorf("exitReason = %s, %q", how, why)
	}
}

// Without GOMAXPROCS in master_environment every component gets the default,
// once; with it, the value given and no default beside it.
func TestGOMAXPROCSDefaultsAndMasterEnvironmentOverrides(t *testing.T) {
	p := newPlanRig(t, "imap", "pop3")
	plan, err := p.chk.Build(p.cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.GOMAXPROCS != DefaultGOMAXPROCS || plan.GOMAXPROCSFrom != "default" {
		t.Errorf("plan says GOMAXPROCS=%s from %s, want the default", plan.GOMAXPROCS, plan.GOMAXPROCSFrom)
	}
	for _, c := range plan.Components {
		if n := countVar(c.Env, "GOMAXPROCS"); n != 1 || envOf(plan, c.Name)["GOMAXPROCS"] != DefaultGOMAXPROCS {
			t.Errorf("%s has GOMAXPROCS %d times, %q; want the default once", c.Name, n, envOf(plan, c.Name)["GOMAXPROCS"])
		}
	}

	p.cfg.Master.Environment = map[string]string{"GOMAXPROCS": "3"}
	plan, err = p.chk.Build(p.cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.GOMAXPROCS != "3" || plan.GOMAXPROCSFrom != "master_environment" {
		t.Errorf("plan says GOMAXPROCS=%s from %s, want 3 from master_environment", plan.GOMAXPROCS, plan.GOMAXPROCSFrom)
	}
	for _, c := range plan.Components {
		if n := countVar(c.Env, "GOMAXPROCS"); n != 1 || envOf(plan, c.Name)["GOMAXPROCS"] != "3" {
			t.Errorf("%s has GOMAXPROCS %d times, %q; want 3 once", c.Name, n, envOf(plan, c.Name)["GOMAXPROCS"])
		}
	}
}

func countVar(env []string, name string) int {
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, name+"=") {
			n++
		}
	}
	return n
}
