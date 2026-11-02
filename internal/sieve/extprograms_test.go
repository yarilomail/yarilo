//go:build unix

package sieve

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// program writes an executable shell script into dir.
func program(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
}

// socketProgram serves one connection on dir/name: it reads the request to
// EOF, keeps it, and answers reply.
func socketProgram(t *testing.T, dir, name, reply string) <-chan string {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "unix", filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		b, _ := io.ReadAll(c)
		got <- string(b)
		_, _ = c.Write([]byte(reply))
	}()
	return got
}

// shortDir is a directory whose path fits a Unix socket address.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "sv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// A pipe program gets the message on stdin, CRLF when asked, its arguments,
// and the envelope in its environment; its failure is reported with what it
// said; a program that is missing, a directory or world-writable never runs.
func TestPipeRunsTheProgram(t *testing.T) {
	bin := t.TempDir()
	out := filepath.Join(t.TempDir(), "out")
	program(t, bin, "keep", `{ cat; echo "|$1|$USER|$SENDER|$RECIPIENT"; } > `+out+"\n")
	program(t, bin, "fail", "echo refused >&2\nexit 3\n")
	program(t, bin, "slow", "sleep 5\n")
	if err := os.Mkdir(filepath.Join(bin, "adir"), 0o700); err != nil {
		t.Fatal(err)
	}
	program(t, bin, "open", "true\n")
	if err := os.Chmod(filepath.Join(bin, "open"), 0o777); err != nil {
		t.Fatal(err)
	}
	e := &pipeExecutor{binDir: bin, timeout: 2 * time.Second, crlf: true,
		username: "u1", envelopeFrom: "from@x", envelopeTo: "to@x"}
	if err := e.Pipe(context.Background(), "keep", []string{"arg"}, strings.NewReader("a\nb\r\n")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); string(b) != "a\r\nb\r\n|arg|u1|from@x|to@x\n" {
		t.Errorf("the program saw %q", b)
	}
	tests := []struct{ name, want string }{
		{"fail", "refused"},
		{"adir", "is a directory"},
		{"open", "world-writable"},
		{"absent", "not found"},
	}
	for _, tc := range tests {
		if err := e.Pipe(context.Background(), tc.name, nil, strings.NewReader("m")); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("pipe %s = %v, want %q", tc.name, err, tc.want)
		}
	}
	e.timeout = 100 * time.Millisecond
	start := time.Now()
	if err := e.Pipe(context.Background(), "slow", nil, strings.NewReader("m")); err == nil || time.Since(start) > 3*time.Second {
		t.Errorf("a program past its timeout = %v after %s, want it stopped", err, time.Since(start))
	}
}

// A socket in socket_dir takes precedence over a program of the same name
// and receives the message.
func TestPipePrefersTheSocket(t *testing.T) {
	sock, bin := shortDir(t), t.TempDir()
	program(t, bin, "svc", "exit 1\n")
	got := socketProgram(t, sock, "svc", "")
	e := &pipeExecutor{binDir: bin, socketDir: sock, timeout: 2 * time.Second}
	if err := e.Pipe(context.Background(), "svc", nil, strings.NewReader("over the socket\n")); err != nil {
		t.Fatal(err)
	}
	if m := <-got; m != "over the socket\n" {
		t.Errorf("the socket got %q", m)
	}
}

// A filter's output replaces the message; a filter that fails or prints
// nothing leaves it alone, as does one that is not there.
func TestFilterReplacesTheMessage(t *testing.T) {
	bin, sock := t.TempDir(), shortDir(t)
	program(t, bin, "upper", "tr a-z A-Z\n")
	program(t, bin, "fails", "echo partial\nexit 1\n")
	program(t, bin, "silent", "cat > /dev/null\n")
	got := socketProgram(t, sock, "remote", "from the socket")
	e := &filterExecutor{binDir: bin, socketDir: sock, timeout: 2 * time.Second, crlf: true}

	read := func(name string) (string, bool) {
		t.Helper()
		r, err := e.Filter(context.Background(), name, nil, strings.NewReader("body\n"))
		if err != nil {
			t.Fatalf("filter %s: %v", name, err)
		}
		if r == nil {
			return "", false
		}
		b, _ := io.ReadAll(r)
		return string(b), true
	}
	if out, ok := read("upper"); !ok || out != "BODY\r\n" {
		t.Errorf("upper = %q, %v", out, ok)
	}
	for _, name := range []string{"fails", "silent", "absent"} {
		if out, ok := read(name); ok {
			t.Errorf("%s replaced the message with %q", name, out)
		}
	}
	if out, ok := read("remote"); !ok || out != "from the socket" {
		t.Errorf("remote = %q, %v", out, ok)
	}
	if m := <-got; m != "body\r\n" {
		t.Errorf("the socket got %q", m)
	}
}

// execute answers the program's output and whether it succeeded; with no
// input the program reads an empty stdin.
func TestExecuteReportsOutcome(t *testing.T) {
	bin, sock := t.TempDir(), shortDir(t)
	program(t, bin, "echoargs", `printf '%s:' "$@"; cat`+"\n")
	program(t, bin, "nonzero", "exit 2\n")
	got := socketProgram(t, sock, "remote", "answer")
	e := &executeExecutor{binDir: bin, socketDir: sock, timeout: 2 * time.Second}

	out, ok, err := e.Execute(context.Background(), "echoargs", []string{"a", "b"}, strings.NewReader("in"))
	if err != nil || !ok || string(out) != "a:b:in" {
		t.Errorf("echoargs = %q, %v, %v", out, ok, err)
	}
	out, ok, err = e.Execute(context.Background(), "echoargs", nil, nil)
	if err != nil || !ok || string(out) != ":" {
		t.Errorf("echoargs without input = %q, %v, %v", out, ok, err)
	}
	if _, ok, err := e.Execute(context.Background(), "nonzero", nil, nil); ok || err != nil {
		t.Errorf("nonzero = %v, %v; want a failed test, not an error", ok, err)
	}
	if _, ok, err := e.Execute(context.Background(), "absent", nil, nil); ok || err != nil {
		t.Errorf("absent = %v, %v; want false without an error", ok, err)
	}
	out, ok, err = e.Execute(context.Background(), "remote", nil, strings.NewReader("q"))
	if err != nil || !ok || string(out) != "answer" {
		t.Errorf("remote = %q, %v, %v", out, ok, err)
	}
	if m := <-got; m != "q" {
		t.Errorf("the socket got %q", m)
	}
	if err := os.Mkdir(filepath.Join(bin, "adir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Execute(context.Background(), "adir", nil, nil); err == nil {
		t.Error("a directory was executed")
	}
}

// A program past its timeout is stopped and the delivery goes on, even when a
// child it started still holds its output open.
func TestEveryProgramKindStopsAtItsTimeout(t *testing.T) {
	bin := t.TempDir()
	program(t, bin, "slow", "sleep 5\n")
	kinds := map[string]func() error{
		"pipe": func() error {
			return (&pipeExecutor{binDir: bin, timeout: 100 * time.Millisecond}).Pipe(context.Background(), "slow", nil, strings.NewReader("m"))
		},
		"filter": func() error {
			_, err := (&filterExecutor{binDir: bin, timeout: 100 * time.Millisecond}).Filter(context.Background(), "slow", nil, strings.NewReader("m"))
			return err
		},
		"execute": func() error {
			_, _, err := (&executeExecutor{binDir: bin, timeout: 100 * time.Millisecond}).Execute(context.Background(), "slow", nil, nil)
			return err
		},
	}
	for name, run := range kinds {
		start := time.Now()
		_ = run()
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s held the delivery for %s past a 100ms timeout", name, d)
		}
	}
}
