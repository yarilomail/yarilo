package master

import (
	"bytes"
	"io"
	"os"
	"sync"
	"syscall"
)

// outPipe is one component's output: it writes w, the log process reads r.
// The supervisor holds both ends for its own life.
type outPipe struct {
	name string
	r, w *os.File
}

// dup returns a new *os.File on the same open pipe end. The supervisor never
// hands its own *os.File to exec or reads it: exec's Fd() would put the
// shared description in blocking mode, and a blocking read cannot be stopped.
// nonblock sets O_NONBLOCK on the description, for a reader that must stop.
func dup(f *os.File, nonblock bool) (*os.File, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return nil, err
	}
	var nfd int
	var derr error
	if err := rc.Control(func(fd uintptr) {
		nfd, derr = syscall.Dup(int(fd))
		if derr == nil && nonblock {
			derr = syscall.SetNonblock(nfd, true)
		}
	}); err != nil {
		return nil, err
	}
	if derr != nil {
		return nil, derr
	}
	syscall.CloseOnExec(nfd)
	return os.NewFile(uintptr(nfd), f.Name()), nil
}

func newOutPipe(name string) (*outPipe, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	return &outPipe{name: name, r: r, w: w}, nil
}

// drainer reads and discards the pipes while no log process reads them, so a
// component never blocks on a full pipe. It stops before a log process
// starts: the supervisor and a log process never read one pipe at once.
type drainer struct {
	s       *Supervisor
	mu      sync.Mutex
	running bool
	readers []*os.File // the drainer's own non-blocking copies, closed to stop it
	wg      sync.WaitGroup
}

func (d *drainer) start() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.running {
		return
	}
	d.running = true
	d.s.log.Error("master: no log process reads the components' output; the supervisor drops it until one runs")
	for _, op := range d.s.pipes {
		f, err := dup(op.r, true)
		if err != nil {
			d.s.log.Error("master: a pipe cannot be drained", "component", op.name, "err", err)
			continue
		}
		d.readers = append(d.readers, f)
		d.wg.Add(1)
		go d.discard(op.name, f)
	}
}

func (d *drainer) discard(name string, f *os.File) {
	defer d.wg.Done()
	buf := make([]byte, 64<<10)
	dropped := d.s.m.logDropped.WithLabelValues(name)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			dropped.Add(float64(bytes.Count(buf[:n], []byte{'\n'})))
		}
		if err != nil {
			return // closed by stop, or the pipe is gone
		}
	}
}

func (d *drainer) stop() {
	d.mu.Lock()
	if !d.running {
		d.mu.Unlock()
		return
	}
	d.running = false
	for _, f := range d.readers {
		_ = f.Close()
	}
	d.readers = nil
	d.mu.Unlock()
	d.wg.Wait()
	d.s.log.Info("master: a log process reads the components' output again")
}

// ownWriter writes the supervisor's own lines into its pipe without ever
// waiting, and into fallback what the pipe does not take.
type ownWriter struct {
	s        *Supervisor
	pipe     *os.File
	fallback io.Writer
	mu       sync.Mutex
}

func (o *ownWriter) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	written := 0
	if o.s.logUp.Load() {
		if rc, err := o.pipe.SyscallConn(); err == nil {
			_ = rc.Write(func(fd uintptr) bool {
				n, werr := syscall.Write(int(fd), p)
				if werr == nil && n > 0 {
					written = n
				}
				return true // never wait for the pipe to drain
			})
		}
	}
	if written < len(p) {
		if _, err := o.fallback.Write(p[written:]); err != nil {
			return written, err
		}
	}
	return len(p), nil
}
