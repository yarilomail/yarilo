package main

import (
	"maps"
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
)

// What yarilo.yaml says the body is reaches the policy client.
func TestThePolicyBodyComesFromTheConfig(t *testing.T) {
	cfg := config.Defaults()
	cfg.Auth.Policy.RequestAttributes = map[string]string{"login": "%{user}", "attrs/ip": "%{local_ip}"}
	if got := policyConfig(cfg).RequestAttributes; !maps.Equal(got, cfg.Auth.Policy.RequestAttributes) {
		t.Errorf("client attributes %v, config %v", got, cfg.Auth.Policy.RequestAttributes)
	}
}
