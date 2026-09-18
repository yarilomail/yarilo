package guard_test

import (
	"os/exec"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// A key that slid one level in during an edit still renders, and the chart only
// fails at apply time: the director carried volumes inside its container (#1906).
func TestEveryWorkloadCarriesItsVolumesOnThePodSpec(t *testing.T) {
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
						Volumes    []map[string]any `yaml:"volumes"`
						Containers []map[string]any `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("parse chart output: %v", err)
		}
		if obj.Kind != "Deployment" && obj.Kind != "StatefulSet" && obj.Kind != "DaemonSet" {
			continue
		}
		checked++
		pod := obj.Spec.Template.Spec
		for _, c := range pod.Containers {
			if _, ok := c["volumes"]; ok {
				t.Errorf("%s: container %v declares volumes, which belong to the pod spec", obj.Metadata.Name, c["name"])
			}
			if c["image"] == nil {
				t.Errorf("%s: container %v has no image, so a key above it is indented one level too deep", obj.Metadata.Name, c["name"])
			}
		}
		if len(pod.Volumes) == 0 {
			t.Errorf("%s: the pod spec declares no volumes, and every workload here mounts its config", obj.Metadata.Name)
		}
	}
	if checked < 5 {
		t.Fatalf("read %d workloads from the chart, which cannot be right", checked)
	}
}
