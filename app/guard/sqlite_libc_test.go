package guard_test

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// modernc.org/sqlite runs only with the libc and memory its own go.mod requires;
// the weekly deps check hides both, so this keeps them in step (#2558).
func TestSqliteLibcPinned(t *testing.T) {
	goMod := goListModule(t, "{{.GoMod}}", "modernc.org/sqlite")
	data, err := os.ReadFile(goMod)
	if err != nil {
		t.Fatalf("read sqlite's go.mod: %v", err)
	}
	for _, mod := range []string{"modernc.org/libc", "modernc.org/memory"} {
		m := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(mod) + `\s+(v\S+)`).FindSubmatch(data)
		if m == nil {
			t.Fatalf("%s: not required by sqlite's go.mod %s", mod, goMod)
		}
		want := string(m[1])
		if got := goListModule(t, "{{.Version}}", mod); got != want {
			t.Errorf("%s %s in the build, sqlite requires %s: set it in go.mod", mod, got, want)
		}
	}
}

func goListModule(t *testing.T, format, mod string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", format, mod).Output()
	if err != nil {
		t.Fatalf("go list -m %s: %v", mod, err)
	}
	return strings.TrimSpace(string(out))
}
