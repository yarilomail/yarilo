package config

import (
	"strings"
	"testing"
)

// A dict opens once per process, so a %-variable in its prefix stays literal
// and every user shares it; the loader refuses it instead of keeping the trap.
func TestADictPrefixHoldsNoVariable(t *testing.T) {
	for _, tc := range []struct {
		name, dicts string
		refuse      bool
	}{
		{"redis prefix with %u refuses", `
  dup:
    driver: redis
    settings: { addr: "r:6379", prefix: "x:dup:%u:" }`, true},
		{"redis prefix without a variable loads", `
  dup:
    driver: redis
    settings: { addr: "r:6379", prefix: "x:dup:" }`, false},
		{"file path with %h loads", `
  meta:
    driver: file
    settings: { path: "%h/meta.json" }`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAML(t, personalNS+"dicts:"+tc.dicts+"\n")
			if !tc.refuse {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), `dict "dup"`) || !strings.Contains(err.Error(), "never expanded") {
				t.Fatalf("err %v; want a refusal naming dict dup", err)
			}
		})
	}
}
