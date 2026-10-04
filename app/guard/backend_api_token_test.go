package guard_test

import (
	"os/exec"
	"strings"
	"testing"
)

// Every backend-api container the chart renders gets its token: one without it
// would refuse to start, and before that refusal it answered anyone.
func TestEveryBackendAPIContainerGetsItsToken(t *testing.T) {
	for _, values := range []string{"", "../../helm_values/values-sandbox.yaml"} {
		args := []string{"template", "t", "../../helm"}
		if values != "" {
			args = append(args, "-f", values)
		}
		out, err := exec.Command("helm", args...).Output()
		if err != nil {
			t.Fatalf("helm template %v: %v", args, err)
		}
		found := 0
		for _, doc := range strings.Split(string(out), "\n---") {
			for i := strings.Index(doc, "- name: yarilo-backend-api\n"); i >= 0; {
				rest := doc[i+1:]
				end := strings.Index(rest, "\n        - name: ")
				block := rest
				if end >= 0 {
					block = rest[:end]
				}
				found++
				if !strings.Contains(block, "name: BACKEND_API_TOKEN") {
					t.Errorf("values %q: a yarilo-backend-api container has no BACKEND_API_TOKEN", values)
				}
				next := strings.Index(rest, "- name: yarilo-backend-api\n")
				if next < 0 {
					break
				}
				i = i + 1 + next
			}
		}
		if found == 0 {
			t.Errorf("values %q: no yarilo-backend-api container rendered; the guard looks at nothing", values)
		}
	}
}
