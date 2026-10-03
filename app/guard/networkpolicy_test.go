package guard_test

import (
	"os/exec"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/yarilomail/yarilo/pkg/mtls"
)

type netpolDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name        string            `yaml:"name"`
		Annotations map[string]string `yaml:"annotations"`
		Labels      map[string]string `yaml:"labels"`
	} `yaml:"metadata"`
	Spec struct {
		PodSelector struct {
			MatchLabels map[string]string `yaml:"matchLabels"`
		} `yaml:"podSelector"`
		Ingress []struct {
			Ports []struct {
				Port int `yaml:"port"`
			} `yaml:"ports"`
			From []map[string]any `yaml:"from"`
		} `yaml:"ingress"`
		Template struct {
			Metadata struct {
				Labels map[string]string `yaml:"labels"`
			} `yaml:"metadata"`
			Spec struct {
				Containers []struct {
					Ports []struct {
						ContainerPort int `yaml:"containerPort"`
					} `yaml:"ports"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

const componentLabel = "app.kubernetes.io/component"

// rolePod is where a role runs in each layout; "" reaches its server over
// loopback or does not exist there.
func rolePod(r mtls.Role, coLocated bool) string {
	switch r {
	case mtls.RoleDirectorAdmin:
		return ""
	case mtls.RoleAdmin:
		if coLocated {
			return "backend"
		}
		return "backend-api"
	case mtls.RoleBackendReg:
		if coLocated {
			return "backend"
		}
		return ""
	case mtls.RoleIMAP, mtls.RolePOP3, mtls.RoleLMTP, mtls.RoleManageSieve, mtls.RoleSubmission,
		mtls.RoleJMAP, mtls.RoleFTS, mtls.RoleBackendAPI:
		if coLocated {
			return "backend"
		}
	}
	return string(r)
}

// listenerPod is the pod serving each listener.
func listenerPod(l mtls.Listener, coLocated bool) string {
	switch l {
	case mtls.ListenerAuthClient, mtls.ListenerAuthMaster:
		return "auth"
	case mtls.ListenerDirector, mtls.ListenerDirectorAPI:
		return "director"
	case mtls.ListenerWarden, mtls.ListenerLocks, mtls.ListenerDict:
		return string(l)
	}
	if coLocated {
		return "backend"
	}
	switch l {
	case mtls.ListenerBackendAPI:
		return "backend-api"
	case mtls.ListenerFTS:
		return "fts"
	}
	return strings.TrimSuffix(string(l), "-backend")
}

// Policies say what pkg/mtls says, both ways: each admits exactly its roles' pods,
// and each rendered server has its policy on ports it opens (#2138).
func TestNetworkPoliciesAreTheRoleMatrix(t *testing.T) {
	for _, tc := range []struct {
		name      string
		coLocated bool
		args      []string
	}{
		{"sandbox, co-located", true, []string{"-f", "../../helm_values/values-sandbox.yaml"}},
		{"sandbox, separate backends", false, []string{"-f", "../../helm_values/values-sandbox.yaml", "--set", "components.backend.coLocated=false",
			"--set", "components.imap.enabled=true", "--set", "components.pop3.enabled=true", "--set", "components.lmtp.enabled=true",
			"--set", "components.submission.enabled=true", "--set", "components.manageSieve.enabled=true", "--set", "components.jmap.enabled=true",
			"--set", "components.backendAPI.enabled=true", "--set", "components.fts.enabled=true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := exec.Command("helm", append([]string{"template", "yarilo", "../../helm"}, tc.args...)...).Output()
			if err != nil {
				t.Fatalf("helm template: %v", err)
			}
			policies := map[mtls.Listener]netpolDoc{}
			podPorts := map[string][]int{}
			dec := yaml.NewDecoder(strings.NewReader(string(out)))
			for {
				var d netpolDoc
				if err := dec.Decode(&d); err != nil {
					break
				}
				switch d.Kind {
				case "NetworkPolicy":
					if l := d.Metadata.Annotations["yarilo.io/listener"]; l != "" {
						policies[mtls.Listener(l)] = d
					}
				case "Deployment", "StatefulSet":
					pod := d.Spec.Template.Metadata.Labels[componentLabel]
					for _, c := range d.Spec.Template.Spec.Containers {
						for _, p := range c.Ports {
							podPorts[pod] = append(podPorts[pod], p.ContainerPort)
						}
					}
				}
			}
			for l := range policies {
				if !slices.Contains(mtls.Listeners, l) {
					t.Errorf("policy for %q, which pkg/mtls has no listener of", l)
				}
			}
			for _, l := range mtls.Listeners {
				server := listenerPod(l, tc.coLocated)
				p, ok := policies[l]
				if _, rendered := podPorts[server]; !rendered {
					if ok {
						t.Errorf("%s: a policy, but its server %q does not render", l, server)
					}
					continue
				}
				if !ok {
					t.Errorf("%s: server %q renders with no policy", l, server)
					continue
				}
				if got := p.Spec.PodSelector.MatchLabels[componentLabel]; got != server {
					t.Errorf("%s: policy selects %q, want %q", l, got, server)
				}
				var want []string
				for _, r := range mtls.Allowed(l) {
					if pod := rolePod(r, tc.coLocated); pod != "" && !slices.Contains(want, pod) {
						want = append(want, pod)
					}
				}
				var got []string
				for _, in := range p.Spec.Ingress {
					for _, port := range in.Ports {
						if !slices.Contains(podPorts[server], port.Port) {
							t.Errorf("%s: policy opens %d, which %q does not listen on (%v)", l, port.Port, server, podPorts[server])
						}
					}
					for _, from := range in.From {
						ps, ok := from["podSelector"].(map[string]any)
						if !ok {
							continue
						}
						if ml, ok := ps["matchLabels"].(map[string]any); ok {
							if c, ok := ml[componentLabel].(string); ok {
								got = append(got, c)
							}
						}
					}
				}
				sort.Strings(got)
				sort.Strings(want)
				if !slices.Equal(got, want) {
					t.Errorf("%s: policy admits %v, the matrix says %v", l, got, want)
				}
			}
			if len(policies) < 8 {
				t.Fatalf("%d listener policies rendered; the guard looks at nothing", len(policies))
			}
		})
	}
}
