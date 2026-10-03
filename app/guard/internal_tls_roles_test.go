package guard_test

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/yarilomail/yarilo/pkg/mtls"
)

type k8sDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name        string            `yaml:"name"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Data map[string]string `yaml:"data"`
	Spec struct {
		SecretName string   `yaml:"secretName"`
		DNSNames   []string `yaml:"dnsNames"`
		Template   struct {
			Spec struct {
				Containers []struct {
					Name string `yaml:"name"`
					Env  []struct {
						Name  string `yaml:"name"`
						Value string `yaml:"value"`
					} `yaml:"env"`
					VolumeMounts []struct {
						Name      string `yaml:"name"`
						MountPath string `yaml:"mountPath"`
					} `yaml:"volumeMounts"`
				} `yaml:"containers"`
				Volumes []struct {
					Name   string `yaml:"name"`
					Secret *struct {
						SecretName string `yaml:"secretName"`
					} `yaml:"secret"`
				} `yaml:"volumes"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

func renderDocs(t *testing.T, args ...string) []k8sDoc {
	t.Helper()
	out, err := exec.Command("helm", append([]string{"template", "yarilo", "../../helm"}, args...)...).Output()
	if err != nil {
		t.Fatalf("helm template %v: %v", args, err)
	}
	var docs []k8sDoc
	dec := yaml.NewDecoder(strings.NewReader(string(out)))
	for {
		var d k8sDoc
		if err := dec.Decode(&d); err != nil {
			break
		}
		if d.Kind != "" {
			docs = append(docs, d)
		}
	}
	return docs
}

func pemCert(t *testing.T, b64 string) *x509.Certificate {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(raw)
	if blk == nil {
		t.Fatal("no PEM block")
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Each internal certificate carries its container's own role, chains to its CA and
// has the pinned name; admin is backend-api only (#2132). Shape, not key stability.
func TestEveryContainerPresentsItsOwnRole(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"sandbox", []string{"-f", "../../helm_values/values-sandbox.yaml"}},
		{"default with internal TLS", []string{"--set", "components.auth.internalTLS.enabled=true",
			"--set", "components.warden.internalTLS.enabled=true", "--set", "components.backend.internalTLS.enabled=true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docs := renderDocs(t, tc.args...)
			secrets := map[string]k8sDoc{}
			certs := map[string]k8sDoc{}
			for _, d := range docs {
				switch d.Kind {
				case "Secret":
					secrets[d.Metadata.Name] = d
				case "Certificate":
					certs[d.Spec.SecretName] = d
				}
			}
			checked := 0
			for _, d := range docs {
				if d.Kind != "Deployment" && d.Kind != "StatefulSet" {
					continue
				}
				pod := d.Spec.Template.Spec
				volSecret := map[string]string{}
				for _, v := range pod.Volumes {
					if v.Secret != nil {
						volSecret[v.Name] = v.Secret.SecretName
					}
				}
				for _, c := range pod.Containers {
					var component string
					for _, e := range c.Env {
						if e.Name == "YARILO_COMPONENT" {
							component = e.Value
						}
					}
					role := mtls.Role(strings.TrimPrefix(component, "yarilo-"))
					for _, m := range c.VolumeMounts {
						if m.MountPath == "/etc/yarilo/admin-tls" && component != "yarilo-backend-api" {
							t.Errorf("%s/%s mounts the admin certificate", d.Metadata.Name, c.Name)
						}
						if m.MountPath != "/etc/yarilo/internal-tls" {
							continue
						}
						if !slices.Contains(mtls.Roles, role) {
							t.Errorf("%s/%s mounts an internal certificate but is not a role (%q)", d.Metadata.Name, c.Name, component)
							continue
						}
						checked++
						name := volSecret[m.Name]
						if s, ok := secrets[name]; ok {
							leaf := pemCert(t, s.Data["tls.crt"])
							got, err := mtls.RoleOf(leaf)
							if err != nil || got != role {
								t.Errorf("%s/%s: secret %s carries role %q (%v), want %q", d.Metadata.Name, c.Name, name, got, err, role)
							}
							if !slices.Contains(leaf.DNSNames, "yarilo-internal") {
								t.Errorf("secret %s lacks the pinned name yarilo-internal: %v", name, leaf.DNSNames)
							}
							pool := x509.NewCertPool()
							pool.AddCert(pemCert(t, s.Data["ca.crt"]))
							if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
								t.Errorf("secret %s does not chain to its ca.crt as a client: %v", name, err)
							}
							continue
						}
						if cert, ok := certs[name]; ok {
							if !slices.Contains(cert.Spec.DNSNames, string(role)+mtls.RoleSuffix) {
								t.Errorf("%s/%s: Certificate for %s lacks %s%s: %v", d.Metadata.Name, c.Name, name, role, mtls.RoleSuffix, cert.Spec.DNSNames)
							}
							continue
						}
						t.Errorf("%s/%s mounts secret %q, which the chart neither makes nor certifies", d.Metadata.Name, c.Name, name)
					}
				}
			}
			if checked < 5 {
				t.Fatalf("checked %d containers; the guard looks at nothing", checked)
			}
		})
	}
}
