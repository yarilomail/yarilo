package guard_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Both admin APIs start only through their config gate.
func TestTheAdminAPIsStartThroughTheirGate(t *testing.T) {
	for path, call := range map[string]string{
		"../yarilo-director/main.go":    "cfg.DirectorService.API.Gate()",
		"../yarilo-backend-api/main.go": "cfg.BackendAPI.Gate()",
	} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), call) {
			t.Errorf("%s does not call %s", path, call)
		}
	}
}

// A compiled test binary never belongs in the tree.
func TestNoTestBinaryIsTracked(t *testing.T) {
	out, err := exec.Command("git", "ls-files", "--", ":(top,glob)**/*.test").Output()
	if err != nil {
		t.Skipf("git ls-files: %v", err)
	}
	if files := strings.TrimSpace(string(out)); files != "" {
		t.Fatalf("tracked test binaries:\n%s", files)
	}
}
