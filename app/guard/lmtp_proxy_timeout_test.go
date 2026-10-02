package guard_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
)

func TestTheChartRendersTheLMTPProxyTimeout(t *testing.T) {
	vf := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(vf, []byte("protocol:\n  lmtp:\n    proxy:\n      lmtp_proxy_timeout: 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("helm", "template", "../../helm", "-f", vf).Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	path := filepath.Join(t.TempDir(), "yarilo.yaml")
	if err := os.WriteFile(path, []byte(renderedConfig(t, string(out))), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Protocol.LMTP.Proxy.ProxyTimeout; got != 7 {
		t.Errorf("the pod loads lmtp_proxy_timeout %d, want 7", got)
	}
}
