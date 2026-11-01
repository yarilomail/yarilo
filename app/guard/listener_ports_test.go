package guard_test

import (
	"fmt"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// entryPorts names, for each services entry, the workload whose process reads
// it and the container port that serves it, in each backend shape.
var entryPorts = []struct {
	entry, workload, port, colocatedPort string
}{
	{"imaps_login", "yarilo-imap-login", "imaps", ""},
	{"imap_login", "yarilo-imap-login", "imap", ""},
	{"pop3s_login", "yarilo-pop3-login", "pop3s", ""},
	{"pop3_login", "yarilo-pop3-login", "pop3", ""},
	{"submission_login", "yarilo-submission-login", "submission", ""},
	{"submissions_login", "yarilo-submission-login", "submissions", ""},
	{"lmtp_login", "yarilo-lmtp-login", "lmtp", ""},
	{"managesieve_login", "yarilo-managesieve-login", "managesieve", ""},
	{"jmap_login", "yarilo-jmap-login", "jmap", ""},
	{"imap", "yarilo-imap", "imap", "imap"},
	{"pop3", "yarilo-pop3", "pop3", "pop3"},
	{"submission", "yarilo-submission", "submission", "submission"},
	{"lmtp", "yarilo-lmtp", "lmtp", "lmtp"},
	{"managesieve", "yarilo-managesieve", "managesieve", "sieve"},
	{"jmap", "yarilo-jmap", "jmap", "jmap"},
}

// Every listener its own number, so a template that takes an entry's port or
// state from another component's values is caught.
const distinctPorts = `components:
  imapLogin: {enabled: true, listeners: {imaps: {enabled: %[1]v, containerPort: 21993}, imap: {enabled: %[2]v, containerPort: 21143}}}
  pop3Login: {enabled: true, listeners: {pop3s: {enabled: %[1]v, containerPort: 21995}, pop3: {enabled: %[2]v, containerPort: 21110}}}
  submissionLogin: {enabled: true, listeners: {submission: {enabled: %[1]v, containerPort: 21587}, submissions: {enabled: %[2]v, containerPort: 21465}}}
  lmtpLogin: {enabled: true, listeners: {lmtp: {enabled: %[1]v, containerPort: 21024}}}
  manageSieveLogin: {enabled: true, listeners: {managesieve: {enabled: %[1]v, containerPort: 21190}}}
  jmapLogin: {enabled: true, listeners: {jmap: {enabled: %[1]v, containerPort: 21443}}}
  imap: {enabled: true, listeners: {imap: {enabled: %[2]v, containerPort: 22143}}}
  pop3: {enabled: true, listeners: {pop3: {enabled: %[2]v, containerPort: 22110}}}
  submission: {enabled: true, listeners: {submission: {enabled: %[2]v, containerPort: 22587}}}
  lmtp: {enabled: true, listeners: {lmtp: {enabled: %[2]v, containerPort: 22024}}}
  manageSieve: {enabled: true, listeners: {managesieve: {enabled: %[2]v, containerPort: 22190}}}
  jmap: {enabled: true, listeners: {jmap: {enabled: %[2]v, containerPort: 22443}}}
  backend: {coLocated: %[3]v}
`

// containerPorts maps each Deployment and StatefulSet to its named ports.
func containerPorts(t *testing.T, manifests string) map[string]map[string]int {
	t.Helper()
	out := map[string]map[string]int{}
	for _, doc := range strings.Split(manifests, "\n---\n") {
		var obj struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Ports []struct {
								Name          string `yaml:"name"`
								ContainerPort int    `yaml:"containerPort"`
							} `yaml:"ports"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil || (obj.Kind != "Deployment" && obj.Kind != "StatefulSet") {
			continue
		}
		ports := map[string]int{}
		for _, c := range obj.Spec.Template.Spec.Containers {
			for _, p := range c.Ports {
				ports[p.Name] = p.ContainerPort
			}
		}
		out[obj.Metadata.Name] = ports
	}
	return out
}

// Each process's listener in its pod is the entry it reads in the ConfigMap:
// the same port when the entry is on, no port when it is off (#2415).
func TestEachRolesPodPortIsItsConfigEntry(t *testing.T) {
	for _, colocated := range []bool{true, false} {
		for _, flip := range []bool{true, false} {
			name := fmt.Sprintf("colocated=%v login-tls=%v", colocated, flip)
			out := mustRender(t, fmt.Sprintf(distinctPorts, flip, !flip, colocated))
			cfg := loadRendered(t, out)
			pods := containerPorts(t, string(out))
			byName := map[string]*config.ServiceConfig{}
			for _, e := range cfg.Services.Entries() {
				byName[e.Name] = *e.Svc
			}
			for _, r := range entryPorts {
				workload, port := r.workload, r.port
				if colocated && r.colocatedPort != "" {
					workload, port = "yarilo-backend", r.colocatedPort
				}
				ports, ok := pods[workload]
				if !ok {
					t.Errorf("%s: no workload %s for services.%s", name, workload, r.entry)
					continue
				}
				svc := byName[r.entry]
				got, has := ports[port]
				switch {
				case svc == nil:
					t.Errorf("%s: the ConfigMap has no services.%s", name, r.entry)
				case svc.Enabled && !has:
					t.Errorf("%s: services.%s is on at %d, %s has no %s port", name, r.entry, svc.Port, workload, port)
				case svc.Enabled && got != svc.Port:
					t.Errorf("%s: services.%s is %d, %s declares %s:%d", name, r.entry, svc.Port, workload, port, got)
				case !svc.Enabled && has:
					t.Errorf("%s: services.%s is off, %s declares %s:%d", name, r.entry, workload, port, got)
				}
			}
		}
	}
}
