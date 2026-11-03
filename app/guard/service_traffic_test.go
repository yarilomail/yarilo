package guard_test

import (
	"fmt"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// clientServices are the components whose client Service type is a value; the
// submission backend has its own Service only off the co-located backend.
var clientServices = []string{
	"imapLogin", "pop3Login", "submissionLogin", "jmapLogin", "saslLogin",
	"quotaStatus", "submission", "lmtpLogin", "manageSieveLogin",
}

type renderedService struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec map[string]any `yaml:"spec"`
}

func services(t *testing.T, manifests []byte) []renderedService {
	t.Helper()
	var out []renderedService
	dec := yaml.NewDecoder(strings.NewReader(string(manifests)))
	for {
		var s renderedService
		if err := dec.Decode(&s); err != nil {
			break
		}
		if s.Kind == "Service" {
			out = append(out, s)
		}
	}
	return out
}

func allClientServices(serviceType string) string {
	var b strings.Builder
	b.WriteString("components:\n  backend: {coLocated: false}\n")
	for _, c := range clientServices {
		fmt.Fprintf(&b, "  %s: {enabled: true, service: {type: %s}}\n", c, serviceType)
	}
	return b.String()
}

// Every Service rendered as LoadBalancer carries its address families and keeps
// the client address; a ClusterIP Service never carries externalTrafficPolicy,
// which the API server refuses there; empty ipFamilies is left out.
func TestLoadBalancerServicesKeepTheClientAddress(t *testing.T) {
	for _, serviceType := range []string{"LoadBalancer", "ClusterIP"} {
		svcs := services(t, mustRender(t, allClientServices(serviceType)))
		lbs := 0
		for _, s := range svcs {
			name := s.Metadata.Name
			if _, ok := s.Spec["ipFamilies"]; ok {
				t.Errorf("%s: %s renders ipFamilies from an empty list", serviceType, name)
			}
			etp, hasETP := s.Spec["externalTrafficPolicy"]
			if s.Spec["type"] != "LoadBalancer" {
				if hasETP {
					t.Errorf("%s: %s is %v and carries externalTrafficPolicy", serviceType, name, s.Spec["type"])
				}
				continue
			}
			lbs++
			if got := s.Spec["ipFamilyPolicy"]; got != "PreferDualStack" {
				t.Errorf("%s: %s ipFamilyPolicy %v, want PreferDualStack", serviceType, name, got)
			}
			if etp != "Local" {
				t.Errorf("%s: %s externalTrafficPolicy %v, want Local", serviceType, name, etp)
			}
		}
		if want := map[string]int{"LoadBalancer": len(clientServices), "ClusterIP": 0}[serviceType]; lbs != want {
			t.Errorf("%s: %d LoadBalancer Services, want %d", serviceType, lbs, want)
		}
	}
}

// The values reach the Service: a template that drops or hardcodes a key is caught.
func TestServiceTrafficValuesReachTheService(t *testing.T) {
	var b strings.Builder
	b.WriteString("components:\n  backend: {coLocated: false}\n")
	for _, c := range clientServices {
		fmt.Fprintf(&b, "  %s: {enabled: true, service: {type: LoadBalancer, ipFamilyPolicy: RequireDualStack, ipFamilies: [IPv6, IPv4], externalTrafficPolicy: Cluster}}\n", c)
	}
	for _, s := range services(t, mustRender(t, b.String())) {
		if s.Spec["type"] != "LoadBalancer" {
			continue
		}
		name := s.Metadata.Name
		if got := s.Spec["ipFamilyPolicy"]; got != "RequireDualStack" {
			t.Errorf("%s ipFamilyPolicy %v, want RequireDualStack", name, got)
		}
		if got := fmt.Sprint(s.Spec["ipFamilies"]); got != "[IPv6 IPv4]" {
			t.Errorf("%s ipFamilies %s, want [IPv6 IPv4]", name, got)
		}
		if got := s.Spec["externalTrafficPolicy"]; got != "Cluster" {
			t.Errorf("%s externalTrafficPolicy %v, want Cluster", name, got)
		}
	}
}
