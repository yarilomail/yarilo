package main

import "testing"

// The single binary finds its config where every other binary does (#2428).
func TestConfigPath(t *testing.T) {
	t.Setenv("CONFIG", "")
	if got := configPath(); got != "/etc/yarilo/yarilo.yaml" {
		t.Errorf("without CONFIG: %q, want /etc/yarilo/yarilo.yaml", got)
	}
	t.Setenv("CONFIG", "/srv/yarilo.yaml")
	if got := configPath(); got != "/srv/yarilo.yaml" {
		t.Errorf("with CONFIG: %q, want /srv/yarilo.yaml", got)
	}
}
