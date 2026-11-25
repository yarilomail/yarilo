package master

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/sockaddr"
)

// ControlSocket is the control socket's path in a run directory.
func ControlSocket(runDir string) string { return runDir + "/master.sock" }

// Serve answers the control socket until ctx ends. Only allowedUID may use
// it: the mode 0600 says so to the file system, the peer check to the
// connection.
func (s *Supervisor) Serve(ctx context.Context, path string, allowedUID uint32) error {
	_ = os.Remove(path)
	ln, err := new(net.ListenConfig).Listen(ctx, "unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return err
	}
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.answer(conn, allowedUID)
	}
}

func (s *Supervisor) answer(conn net.Conn, allowedUID uint32) {
	defer func() { _ = conn.Close() }()
	uid, err := sockaddr.PeerUID(conn)
	if err != nil || uid != allowedUID {
		_, _ = fmt.Fprintf(conn, "ERR not allowed\n")
		return
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Minute)) // a stop may wait out every stop timeout
	line, err := bufio.NewReader(io.LimitReader(conn, 4096)).ReadString('\n')
	if err != nil && line == "" {
		return
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		_, _ = fmt.Fprintf(conn, "ERR empty command\n")
		return
	}
	cmd, names := fields[0], fields[1:]
	var cerr error
	switch cmd {
	case "status":
		// With names, only those; and the answer says whether each of them
		// runs, for `service yarilo status <name>` to exit by it.
		sts, serr := s.statusOf(names)
		if serr != nil {
			cerr = serr
			break
		}
		down := []string{}
		for _, st := range sts {
			_, _ = fmt.Fprintln(conn, FormatStatus(st))
			if st.State != StateRunning {
				down = append(down, st.Name)
			}
		}
		if len(down) > 0 {
			cerr = fmt.Errorf("not running: %s", strings.Join(down, " "))
		}
	case "stop":
		cerr = s.StopComponents(names...)
	case "start":
		cerr = s.StartComponents(names...)
	case "restart":
		cerr = s.Restart(names...)
	case "reopen":
		cerr = s.Reopen()
	default:
		cerr = fmt.Errorf("unknown command %q", cmd)
	}
	if cerr != nil {
		_, _ = fmt.Fprintf(conn, "ERR %s\n", strings.ReplaceAll(cerr.Error(), "\n", "; "))
		return
	}
	_, _ = fmt.Fprintf(conn, "OK\n")
}

func (s *Supervisor) statusOf(names []string) ([]Status, error) {
	all := s.Status()
	if len(names) == 0 {
		return all, nil
	}
	by := map[string]Status{}
	for _, st := range all {
		by[st.Name] = st
	}
	out := make([]Status, 0, len(names))
	for _, n := range names {
		st, ok := by[n]
		if !ok {
			return nil, fmt.Errorf("no component %q", n)
		}
		out = append(out, st)
	}
	return out, nil
}

// FormatStatus is one tab-separated status line: name, state, pid or -,
// uptime in seconds, restarts, why or -.
func FormatStatus(st Status) string {
	pid, why := "-", "-"
	if st.PID > 0 {
		pid = strconv.Itoa(st.PID)
	}
	if st.Why != "" {
		why = strings.ReplaceAll(st.Why, "\t", " ")
	}
	return strings.Join([]string{st.Name, string(st.State), pid,
		strconv.Itoa(int(st.Uptime.Seconds())), strconv.Itoa(st.Restarts), why}, "\t")
}

// Ctl sends one command to a supervisor and copies its answer to out. The
// error says whether the supervisor refused it.
func Ctl(ctx context.Context, path string, args []string, out io.Writer) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return fmt.Errorf("no yarilo-master on %s: %w", path, err)
	}
	defer func() { _ = conn.Close() }()
	// A refused peer is answered before its command is read, so the write
	// may fail; the answer is still there to read.
	_, werr := fmt.Fprintln(conn, strings.Join(args, " "))
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "OK":
			return nil
		case strings.HasPrefix(line, "ERR "):
			return errors.New(strings.TrimPrefix(line, "ERR "))
		}
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	if werr != nil {
		return werr
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("the supervisor closed the connection without an answer")
}
