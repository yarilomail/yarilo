package guard_test

import (
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// Components that open a Redis client or a redis dict.
var redisUsers = []string{"yarilo-auth", "yarilo-warden", "yarilo-locks", "yarilo-dict", "yarilo-quota-status",
	"yarilo-backend-api", "yarilo-imap", "yarilo-pop3", "yarilo-lmtp", "yarilo-managesieve", "yarilo-submission",
	"yarilo-jmap", "yarilo-fts"}

// With redis.passwordSecret set, the password reaches every Redis user as
// YARILO_REDIS_PASSWORD and the ConfigMap names it only by that variable.
func TestRedisPasswordComesFromTheSecret(t *testing.T) {
	out, err := exec.Command("helm", "template", "yarilo", "../../helm", "-f", "../../helm_values/values-sandbox.yaml").Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	if regexp.MustCompile(`redis://[^"/\s]*@`).Match(out) {
		t.Error("a Redis URL with credentials is rendered")
	}
	for _, key := range []string{"redis_password", "password"} {
		if !strings.Contains(string(out), key+`: "${YARILO_REDIS_PASSWORD}"`) {
			t.Errorf("no %s naming ${YARILO_REDIS_PASSWORD} in the config", key)
		}
	}
	type container struct {
		Env []struct {
			Name  string `yaml:"name"`
			Value string `yaml:"value"`
		} `yaml:"env"`
	}
	var doc struct {
		Kind string `yaml:"kind"`
		Spec struct {
			Template struct {
				Spec struct {
					Containers []container `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	seen := 0
	dec := yaml.NewDecoder(strings.NewReader(string(out)))
	for dec.Decode(&doc) == nil {
		for _, c := range doc.Spec.Template.Spec.Containers {
			var component string
			has := false
			for _, e := range c.Env {
				if e.Name == "YARILO_COMPONENT" {
					component = e.Value
				}
				if e.Name == "YARILO_REDIS_PASSWORD" {
					has = true
				}
			}
			if slices.Contains(redisUsers, component) {
				seen++
				if !has {
					t.Errorf("%s opens Redis without YARILO_REDIS_PASSWORD", component)
				}
			}
		}
		doc.Spec.Template.Spec.Containers = nil
	}
	if seen < 6 {
		t.Fatalf("checked %d Redis users; the guard looks at nothing", seen)
	}
}
