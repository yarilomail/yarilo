package guard_test

import (
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type sharedLBService struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name        string            `yaml:"name"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Spec struct {
		Type                  string            `yaml:"type"`
		ExternalTrafficPolicy string            `yaml:"externalTrafficPolicy"`
		Selector              map[string]string `yaml:"selector"`
	} `yaml:"spec"`
}

// MetalLB gives one address to several Services only when all of them are
// Cluster or all select the same pods; any other Service in the group stays
// pending. A kept values file that shares an address against that rule takes
// the stand's client ports down on the next deploy (acc1751).
func TestSharedLoadBalancerAddressesCanBeShared(t *testing.T) {
	const sandbox = "../../helm_values/values-sandbox.yaml"
	sets := [][]string{{sandbox}}
	overlays, err := filepath.Glob("../../helm_values/values-sandbox-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range overlays {
		sets = append(sets, []string{sandbox, o})
	}
	examples, err := filepath.Glob("../../helm_values/examples/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range examples {
		sets = append(sets, []string{e})
	}
	shared := 0
	for _, set := range sets {
		t.Run(filepath.Base(set[len(set)-1]), func(t *testing.T) {
			args := []string{"template", "yarilo", "../../helm"}
			for _, f := range set {
				args = append(args, "-f", f)
			}
			out, err := exec.CommandContext(t.Context(), "helm", args...).Output()
			if err != nil {
				t.Fatalf("helm template: %v", err)
			}
			groups := map[string][]sharedLBService{}
			dec := yaml.NewDecoder(strings.NewReader(string(out)))
			for {
				var s sharedLBService
				if err := dec.Decode(&s); err != nil {
					break
				}
				if s.Kind != "Service" || s.Spec.Type != "LoadBalancer" {
					continue
				}
				for _, k := range []string{"metallb.universe.tf/allow-shared-ip", "metallb.io/allow-shared-ip"} {
					if key := s.Metadata.Annotations[k]; key != "" {
						groups[key] = append(groups[key], s)
					}
				}
			}
			for key, svcs := range groups {
				if len(svcs) < 2 {
					continue
				}
				shared++
				allCluster, sameSelector := true, true
				var names []string
				for _, s := range svcs {
					names = append(names, s.Metadata.Name+"="+s.Spec.ExternalTrafficPolicy)
					allCluster = allCluster && s.Spec.ExternalTrafficPolicy == "Cluster"
					sameSelector = sameSelector && reflect.DeepEqual(s.Spec.Selector, svcs[0].Spec.Selector)
				}
				if !allCluster && !sameSelector {
					sort.Strings(names)
					t.Errorf("address %q is shared by Services with different pods that are not all Cluster: %v", key, names)
				}
			}
		})
	}
	if shared == 0 {
		t.Fatal("no values file shares an address: the row would pass by reading nothing")
	}
}
