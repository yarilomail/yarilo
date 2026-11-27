package guard_test

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// keyless are the binaries that build the store with no keys: they read sizes
// from the index and never a body, so the chart mounts none into them.
// TestAKeylessBinaryStartsWithEncryptionOn holds each entry by a real start.
var keyless = map[string]string{
	"yarilo-quota-status": "the MTA's policy check: sizes from the index, no body opened",
}

// A keyless binary starts under write: age and reference_keys with no key
// file in its container, which is how the chart runs it.
func TestAKeylessBinaryStartsWithEncryptionOn(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "no-keys")
	cfg := filepath.Join(dir, "yarilo.yaml")
	body := "storage:\n  mail_temp_dir: " + filepath.Join(dir, "tmp") + "\n  mail_crypt:\n    write: age\n" +
		"    public_key: " + missing + "/public\n    private_keys: " + missing + "/private\n" +
		"    reference_keys: [" + missing + "/reference.pem]\n" +
		"quota_status:\n  listen: 127.0.0.1:0\ntelemetry:\n  listen: 127.0.0.1:0\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for bin := range keyless {
		t.Run(bin, func(t *testing.T) {
			exe := filepath.Join(dir, bin)
			if out, err := exec.CommandContext(t.Context(), "go", "build", "-o", exe, "../"+bin).CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			cmd := exec.CommandContext(t.Context(), exe)
			cmd.Env = append(os.Environ(), "CONFIG="+cfg)
			logs, err := cmd.StderrPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stdout = cmd.Stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			started := make(chan string, 1)
			go func() {
				var seen []string
				sc := bufio.NewScanner(logs)
				for sc.Scan() {
					seen = append(seen, sc.Text())
					if strings.Contains(sc.Text(), bin+" starting") {
						started <- ""
						return
					}
				}
				started <- strings.Join(seen, "\n")
			}()
			select {
			case out := <-started:
				if out != "" {
					t.Fatalf("%s exited before it started:\n%s", bin, out)
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("%s did not start in 30 s", bin)
			}
		})
	}
}
