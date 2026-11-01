package guard_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// Every process starts its telemetry with Start and stops on its error. A
// ListenAndServe in a goroutine logs a taken port and leaves the process
// running without /healthz and /readyz (#2419).
func TestTelemetryIsStartedNotServedFromAGoroutine(t *testing.T) {
	files, err := filepath.Glob("../yarilo*/*.go")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, "../../internal/backend/backend.go")
	served := regexp.MustCompile(`(tel|telem)\.ListenAndServe\(`)
	for _, f := range files {
		if filepath.Base(f) != "main.go" && filepath.Base(f) != "backend.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if served.Match(src) {
			t.Errorf("%s serves telemetry with ListenAndServe; start it with Start and stop on its error", f)
		}
	}
}
