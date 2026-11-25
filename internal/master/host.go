package master

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// maxComm is how much of a command name the kernel keeps (MAXCOMLEN).
const maxComm = 19

// IsOurs reports whether pid still runs the component's binary, by the
// command name the kernel keeps for it.
func IsOurs(pid int, c Component) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	want := "yarilo-" + c.Name
	if len(want) > maxComm {
		want = want[:maxComm]
	}
	return strings.TrimSpace(string(out)) == want
}
