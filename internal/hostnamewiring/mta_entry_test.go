package hostnamewiring

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// No manifest sends an MTA to the director: mail enters through the LMTP login
// service, and a director address on that port refuses every delivery (#1756).
func TestNoManifestSendsMailToTheDirector(t *testing.T) {
	root := filepath.Join("..", "..")
	dirs := []string{
		filepath.Join(root, "helm", "templates"),
		filepath.Join(root, "helm_values"),
	}
	var checked int
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			path := filepath.Join(dir, e.Name())
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatalf("read %s: %v", path, rerr)
			}
			checked++
			text := string(src)
			for _, forbidden := range []string{
				"director-lmtp",
				"director.listeners.lmtp",
				"lmtp_listen",
				"lmtp_backend_port",
			} {
				if strings.Contains(text, forbidden) {
					t.Errorf("%s still names %q: the director does not serve LMTP", e.Name(), forbidden)
				}
			}
		}
	}
	// A guard that reads nothing has stopped guarding.
	if checked < 10 {
		t.Errorf("read %d manifests; this guard is looking at the wrong place", checked)
	}
}
