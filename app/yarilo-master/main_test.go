package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/master"
)

// ctl status answers on stdout alone: a config warning goes to stderr, so a
// script reading a status field never gets a line of JSON instead.
func TestCtlAnswersOnStdoutAndLogsOnStderr(t *testing.T) {
	dir, err := os.MkdirTemp("", "mc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	run, logs := filepath.Join(dir, "run"), filepath.Join(dir, "log")
	for _, d := range []string{run, logs} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	plan := &master.Plan{
		RunDir: run, LogPath: logs, StopTimeout: time.Second,
		Components: []master.Component{{Name: "imap", Bin: "/bin/sleep"}},
	}
	sup, err := master.New(plan, master.Options{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sup.Stop)
	go sup.Run()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = sup.Serve(ctx, master.ControlSocket(run), uint32(os.Getuid())) }()
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(master.ControlSocket(run)); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// storage.index is a retired key: loading the file warns about it.
	cfg := filepath.Join(dir, "yarilo.yaml")
	body := "storage:\n  index: file\nmaster:\n  master_run_dir: " + run + "\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG", cfg)

	var stdout, stderr bytes.Buffer
	realMain([]string{"ctl", "status"}, &stdout, &stderr)
	if !strings.Contains(stderr.String(), "retired key") {
		t.Fatalf("the config warning did not reach stderr: %q", stderr.String())
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout holds %d lines, want the one status line: %q", len(lines), stdout.String())
	}
	if f := strings.Split(lines[0], "\t"); len(f) != 6 || f[0] != "imap" {
		t.Errorf("stdout line %q is not a status line", lines[0])
	}

	stdout.Reset()
	stderr.Reset()
	realMain([]string{"check"}, &stdout, &stderr)
	if strings.Contains(stdout.String(), "{") {
		t.Errorf("check wrote a log line to stdout: %q", stdout.String())
	}
}

// check says which GOMAXPROCS the components will get and where it comes from.
func TestCheckNamesTheGOMAXPROCSTheComponentsGet(t *testing.T) {
	for _, tc := range []struct{ value, from, want string }{
		{"2", "default", "GOMAXPROCS=2 (default)"},
		{"3", "master_environment", "GOMAXPROCS=3 (master_environment)"},
	} {
		if got := checkLine(&master.Plan{GOMAXPROCS: tc.value, GOMAXPROCSFrom: tc.from}); !strings.Contains(got, tc.want) {
			t.Errorf("checkLine = %q, want it to say %q", got, tc.want)
		}
	}
}
