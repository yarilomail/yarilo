package main

import (
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// The prefixes config.Defaults writes are the ones an empty value gave, byte
// for byte: a difference would part the keys in Valkey across an upgrade.
func TestDefaultsAreTheEmptyFallback(t *testing.T) {
	d := config.Defaults().WardenService
	key, ch := redisPrefixes(config.WardenServiceConfig{})
	if d.KeyPrefix != key || d.ChannelPrefix != ch {
		t.Fatalf("defaults %q %q, empty gave %q %q", d.KeyPrefix, d.ChannelPrefix, key, ch)
	}
	if got, _ := redisPrefixes(d); got != key {
		t.Fatalf("the defaults resolve to %q", got)
	}
	if d.StateBackend == "redis" {
		t.Fatal("the default state backend is Redis; empty was memory")
	}
}
