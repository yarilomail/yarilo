package filelock

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// probeEnv carries the probe request to the child: the same binary, told to
// take one lock and say whether it got it.
const probeEnv = "YARILO_FILELOCK_PROBE"

// ProbeMain runs the child half of Verify and reports whether it ran. Every
// binary that verifies a volume calls it first thing in main: without it the
// child would start a server instead of answering (#1840).
func ProbeMain() bool {
	spec := os.Getenv(probeEnv)
	if spec == "" {
		return false
	}
	path, method, ok := strings.Cut(spec, "|")
	if !ok {
		fmt.Println("probe: bad request")
		os.Exit(2)
	}
	h, err := takeShared(path, Method(method), 200*time.Millisecond)
	if err != nil {
		fmt.Println("busy")
		os.Exit(0)
	}
	_ = h.Release()
	fmt.Println("taken")
	os.Exit(0)
	return true
}

// askAnotherProcess has a child of this binary try the same lock. A POSIX
// record lock belongs to the process, so only another process can answer.
func askAnotherProcess(path string, method Method) (taken bool, err error) {
	self, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("filelock/verify: find this binary: %w", err)
	}
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), probeEnv+"="+path+"|"+string(method))
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("filelock/verify: the probe did not answer: %w", err)
	}
	return strings.TrimSpace(string(out)) == "taken", nil
}
