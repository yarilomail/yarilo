package config

import (
	"os"
	"path/filepath"
	"regexp"
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

// Every service the compose config dials by name is a service of the stack:
// the loader cannot see that an address names a container nobody runs.
func TestTheComposeConfigNamesOnlyServicesTheStackRuns(t *testing.T) {
	dir := filepath.Join("..", "..", "deploy", "compose")
	compose, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := os.ReadFile(filepath.Join(dir, "config", "yarilo.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	services := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^  ([a-z0-9-]+):\s*$`).FindAllSubmatch(compose, -1) {
		services[string(m[1])] = true
	}
	for _, m := range regexp.MustCompile(`"([a-z][a-z0-9-]*):[0-9]{2,5}"`).FindAllSubmatch(cfg, -1) {
		if host := string(m[1]); host != "localhost" && !services[host] {
			t.Errorf("config/yarilo.yaml dials %s, which docker-compose.yml does not run", host)
		}
	}
}
