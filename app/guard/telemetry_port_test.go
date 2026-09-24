package guard_test

import (
	"os/exec"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// Containers share one network namespace, so one without a telemetry port of
// its own is read off a neighbour's page, and its counters read zero (#1999).
func TestEveryBackendContainerHasItsOwnTelemetryPort(t *testing.T) {
	out, err := exec.Command("helm", "template", "../../helm", "-f", "../../helm_values/values-sandbox.yaml").Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	checked := 0
	for _, doc := range strings.Split(string(out), "\n---\n") {
		var obj struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Name string `yaml:"name"`
							Env  []struct {
								Name  string `yaml:"name"`
								Value string `yaml:"value"`
							} `yaml:"env"`
							Ports []struct {
								Name          string `yaml:"name"`
								ContainerPort int    `yaml:"containerPort"`
							} `yaml:"ports"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("parse chart output: %v", err)
		}
		if obj.Kind != "StatefulSet" || !strings.HasSuffix(obj.Metadata.Name, "-backend") {
			continue
		}
		seen := map[int]string{}
		for _, c := range obj.Spec.Template.Spec.Containers {
			checked++
			var port int
			for _, p := range c.Ports {
				if p.Name == "telemetry" {
					port = p.ContainerPort
				}
			}
			if port == 0 {
				t.Errorf("container %q declares no telemetry port, so its counters are read off a neighbour", c.Name)
				continue
			}
			if by, ok := seen[port]; ok {
				t.Errorf("containers %q and %q both claim telemetry port %d", by, c.Name, port)
			}
			seen[port] = c.Name
			// The port is what the process listens on, not only what the pod
			// advertises: the env is what the binary reads.
			var listen string
			for _, e := range c.Env {
				if e.Name == "TELEMETRY_LISTEN" {
					listen = e.Value
				}
			}
			if listen == "" {
				t.Errorf("container %q declares a telemetry port but tells the process nothing", c.Name)
			}
		}
	}
	if checked < 5 {
		t.Fatalf("checked %d containers, which cannot be the backend pod", checked)
	}
}
