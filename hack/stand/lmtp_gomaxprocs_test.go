package stand

import (
	"os/exec"
	"strings"
	"testing"
)

// The deploy leaves yarilo-lmtp's GOMAXPROCS as asked whatever the live object
// held before: helm upgrade keeps an env entry that kubectl set env added, so
// only the step itself can take it away (docs-internal#318).
func TestTheDeployLeavesLmtpGOMAXPROCSAsAsked(t *testing.T) {
	const fake = `
. ./deploy.sh
D=$(mktemp -d); SPEC=$D/spec
kube() {
  case "$1 $2" in
    "get sts") cat "$SPEC" ;;
    "set env") case "$6" in GOMAXPROCS-) : > "$SPEC" ;; GOMAXPROCS=*) printf %s "${6#GOMAXPROCS=}" > "$SPEC" ;; esac; echo "set $6" >> "$D/calls" ;;
    "rollout status") echo rollout >> "$D/calls" ;;
  esac
}
printf %s "$HAVE" > "$SPEC"; : > "$D/calls"
STAND_LMTP_GOMAXPROCS="$WANT" stand_lmtp_gomaxprocs "$D" > /dev/null || exit 1
echo "$(cat "$D/lmtp-gomaxprocs.txt")|$(tr '\n' ' ' < "$D/calls")"
`
	for _, tc := range []struct{ have, want, file, calls string }{
		{"", "", "absent", ""},
		{"2", "", "absent", "set GOMAXPROCS- rollout "},
		{"", "2", "set 2", "set GOMAXPROCS=2 rollout "},
		{"2", "2", "set 2", ""},
		{"4", "2", "set 2", "set GOMAXPROCS=2 rollout "},
	} {
		cmd := exec.CommandContext(t.Context(), "bash", "-c", fake)
		cmd.Env = append(cmd.Environ(), "HAVE="+tc.have, "WANT="+tc.want)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("have %q want %q: %v\n%s", tc.have, tc.want, err, out)
		}
		got := strings.TrimSpace(string(out))
		if want := strings.TrimSpace(tc.file + "|" + tc.calls); got != want {
			t.Errorf("have %q want %q: got %q, want %q", tc.have, tc.want, got, want)
		}
	}
	cmd := exec.CommandContext(t.Context(), "bash", "-c", fake)
	cmd.Env = append(cmd.Environ(), "HAVE=", "WANT=x")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("a GOMAXPROCS that is not a number was taken: %s", out)
	}
}
