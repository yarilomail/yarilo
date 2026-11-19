package stand

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func scenarioSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("crypt-scenarios.sh")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The counts come from the table's own total, so a folder whose name holds a
// space does not shift a column; --all reads its ALL USERS line.
func TestTheScenarioCountsReadTheTotals(t *testing.T) {
	fn := regexp.MustCompile(`(?s)\ncounts\(\) \{.*?\n\}\n`).FindString(scenarioSource(t))
	if fn == "" {
		t.Fatal("no counts() in crypt-scenarios.sh")
	}
	for _, tc := range []struct{ in, want string }{
		{"NAMESPACE  FOLDER  PLAIN  CURRENT  OTHER_KEY  REFERENCE  NO_KEY  FAILED\npersonal  My Box  2  3  0  0  0  1\n  TOTAL  2  3  0  0  0  1\n",
			"plain=2 current=3 other_key=0 reference=0 no_key=0 failed=1"},
		{"ALL USERS  PLAIN  CURRENT  OTHER_KEY  REFERENCE  NO_KEY  FAILED\n150 users  10  20  1  4  0  0\n",
			"plain=10 current=20 other_key=1 reference=4 no_key=0 failed=0"},
		{"error: backend-api unreachable\n", "unread"},
	} {
		cmd := exec.Command("bash", "-c", fn+"counts")
		cmd.Stdin = strings.NewReader(tc.in)
		out, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(out)) != tc.want {
			t.Errorf("counts(%q) = %q, %v; want %q", tc.in, out, err, tc.want)
		}
	}
}

// The keys go only once status --all shows no encrypted message, however the
// run ended; otherwise the stand keeps them with write off.
func TestTheKeysGoOnlyWhenNoMailIsEncrypted(t *testing.T) {
	src := scenarioSource(t)
	if n := strings.Count(src, `mailcrypt-keys.sh" delete`); n != 1 {
		t.Fatalf("the Secret is deleted at %d places, want one", n)
	}
	finish := regexp.MustCompile(`(?s)\nfinish\(\) \{.*?\n\}\n`).FindString(src)
	for _, want := range []string{
		`if [ "$(encrypted "$left")" = 0 ]; then
      deploy restore && bash "$HERE/mailcrypt-keys.sh" delete`,
		`deploy keys-kept -f "$VALUES/values-sandbox-mailcrypt-keys.yaml"`,
	} {
		if !strings.Contains(finish, want) {
			t.Errorf("finish() lacks %s", want)
		}
	}
	if !strings.Contains(src, "trap finish EXIT") {
		t.Error("the cleanup is not on EXIT: a failed scenario would leave the keys and the write mode")
	}
}
