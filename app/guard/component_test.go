package guard_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// A component the chart starts must be built and dispatched: missing either,
// the pod crash-loops with "Unknown YARILO_COMPONENT" (#1733).
func TestEveryChartComponentIsBuiltAndDispatched(t *testing.T) {
	entrypoint := readFile(t, "../../docker/entrypoint.sh")
	dockerfile := readFile(t, "../../docker/Dockerfile")

	// The value paired with YARILO_COMPONENT, not any yarilo-looking value: a
	// service name in a neighbouring env entry is not a binary.
	re := regexp.MustCompile(`name:\s*YARILO_COMPONENT\s*\n\s*value:\s*"?([A-Za-z0-9-]+)"?`)
	var wanted []string
	seen := map[string]bool{}
	for _, path := range chartTemplates(t) {
		for _, m := range re.FindAllStringSubmatch(readFile(t, path), -1) {
			if seen[m[1]] {
				continue
			}
			seen[m[1]] = true
			wanted = append(wanted, m[1])
		}
	}
	if len(wanted) < 5 {
		t.Fatalf("found %d components in the chart, which cannot be right", len(wanted))
	}

	for _, c := range wanted {
		if !strings.Contains(entrypoint, "  "+c+")") {
			t.Errorf("%s is started by the chart and the entrypoint does not dispatch it", c)
		}
		if !builtIn(dockerfile, c) {
			t.Errorf("%s is started by the chart and the image does not build it", c)
		}
	}
}

// builtIn accepts either build loop: the static one and the cgo stage that
// builds yarilo-fts list their binaries differently.
func builtIn(dockerfile, cmd string) bool {
	for _, line := range strings.Split(dockerfile, "\n") {
		f := strings.Fields(strings.TrimSuffix(strings.TrimSpace(line), "\\"))
		for _, w := range f {
			if w == cmd {
				return true
			}
		}
	}
	return false
}

func chartTemplates(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("../../helm/templates")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".yaml") {
			out = append(out, "../../helm/templates/"+e.Name())
		}
	}
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
