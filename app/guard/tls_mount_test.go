package guard_test

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// A service that listens with internal mTLS needs the certificate mounted, or
// it does not come up at all: yarilo-dict crash-looped on exactly this (#1733).
func TestEveryInternalTLSListenerMountsItsCertificate(t *testing.T) {
	out, err := exec.Command("helm", "template", "../../helm", "-f", "../../helm_values/values-sandbox.yaml").Output()
	if err != nil {
		t.Skipf("helm not available: %v", err)
	}
	if !strings.Contains(string(out), "internal_tls:") {
		t.Fatal("the sandbox values no longer enable internal_tls, so this row proves nothing")
	}
	docs := strings.Split(string(out), "\n---\n")
	name := regexp.MustCompile(`(?m)^  name: (\S+)`)

	checked := 0
	for _, doc := range docs {
		if !strings.Contains(doc, "kind: Deployment") && !strings.Contains(doc, "kind: StatefulSet") {
			continue
		}
		if !strings.Contains(doc, "YARILO_COMPONENT") {
			continue
		}
		checked++
		if !strings.Contains(doc, "mountPath: /etc/yarilo/internal-tls") {
			m := name.FindStringSubmatch(doc)
			t.Errorf("%v runs with internal_tls on and has no certificate mounted", m)
		}
	}
	if checked < 5 {
		t.Fatalf("read %d workloads from the chart, which cannot be right", checked)
	}
}
