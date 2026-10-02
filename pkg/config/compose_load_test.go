package config

import (
	"path/filepath"
	"testing"
)

// The compose stack's config goes through the same loader as every values
// file: a key it renders wrong stops every service in that stack at start.
func TestTheComposeConfigLoads(t *testing.T) {
	t.Setenv("BACKEND_API_TOKEN", "compose-test-token")
	cfg, err := Load(filepath.Join("..", "..", "deploy", "compose", "config", "yarilo.yaml"))
	if err != nil {
		t.Fatalf("deploy/compose/config/yarilo.yaml does not load: %v", err)
	}
	if cfg.BackendAPI.Listen != ":9105" || cfg.BackendAPI.Token != "compose-test-token" {
		t.Errorf("backend_api listen %q, token from the environment %v; want :9105 and the BACKEND_API_TOKEN value",
			cfg.BackendAPI.Listen, cfg.BackendAPI.Token == "compose-test-token")
	}
}
