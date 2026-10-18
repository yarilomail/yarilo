package main

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
)

// The login advertises a user's set: sieve_extensions less the global-only.
func TestSieveCapabilityIsTheUsersSet(t *testing.T) {
	c := config.SieveConfig{SieveExtensions: []string{"fileinto", "envelope"}, GlobalExtensions: []string{"fileinto"}}
	if got := sieveCapability(c); got != "envelope" {
		t.Errorf("SIEVE = %q, want \"envelope\"", got)
	}
}
