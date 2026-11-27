package stand

import (
	"os/exec"
	"strings"
	"testing"
)

// The LMTP pair reads the idle twice and says so when the two readings
// disagree: one high reading once made the CPU limit look cheaper than no
// limit, while the raw CPU was the same (docs-internal#318).
func TestTheLMTPPairWarnsWhenItsTwoIdlesDisagree(t *testing.T) {
	const fn = `eval "$(sed -n '/^idle_lines()/,/^}/p' lmtp-pair.sh)"; idle_lines yarilo "$B" "$A" 900 "$ROWS"`
	for _, tc := range []struct {
		before, after string
		warn          bool
		want          string
	}{
		// final2184's pair under the limit: 0.319 before, 0.125 today.
		{"0.319", "0.125", true, "run 1: cpu_s=19.8 over 37s, raw ms a message 22.0, less idle 11.6, ms a message 12.9"},
		{"0.125", "0.130", false, "run 1: cpu_s=19.8 over 37s, raw ms a message 22.0, less idle 15.1, ms a message 16.8"},
		// Apart by more than a quarter but by less than the reading moves.
		{"0.010", "0.020", false, "less idle 19.2"},
	} {
		cmd := exec.CommandContext(t.Context(), "bash", "-c", fn)
		cmd.Env = append(cmd.Environ(), "B="+tc.before, "A="+tc.after, "ROWS=1 100 119.8 37\n")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s/%s: %v\n%s", tc.before, tc.after, err, out)
		}
		got := string(out)
		if strings.Contains(got, "WARNING") != tc.warn {
			t.Errorf("idle %s then %s: warning %v, want %v\n%s", tc.before, tc.after, !tc.warn, tc.warn, got)
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("idle %s then %s: no %q in\n%s", tc.before, tc.after, tc.want, got)
		}
	}
}
