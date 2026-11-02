package main

import (
	"strings"
	"testing"
)

// ring status --all renders the cross-replica verdict in both forms, and the
// director verbs answer their usage on a missing argument.
func TestDirectorRingTopologyAndUsage(t *testing.T) {
	startLiveDirector(t)
	mustYarctl(t, "director", "backends", "add", "10.0.0.7", "--port", "143", "--tag", "a")
	outputFormat = "human"

	tests := []struct {
		args []string
		want []string
	}{
		{[]string{"director", "ring", "status", "--all"}, []string{"ring topology:", "REPLICA", "REACHABLE"}},
	}
	for _, tc := range tests {
		out := mustYarctl(t, tc.args...)
		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("yarctl %s:\n%s\nwant it to contain %q", strings.Join(tc.args, " "), out, w)
			}
		}
	}

	outputFormat = "json"
	var topo struct {
		Replicas []struct {
			Reachable bool `json:"reachable"`
		} `json:"replicas"`
	}
	decode(t, mustYarctl(t, "director", "ring", "status", "--all"), &topo)
	if len(topo.Replicas) == 0 {
		t.Errorf("ring status --all names no replica: %+v", topo)
	}
	for _, args := range [][]string{
		{"director", "ring", "add"}, {"director", "ring", "remove"},
		{"director", "users", "kick"}, {"director", "users", "move"},
		{"director", "domains", "move", "example.com"},
		{"director", "backends", "remove"}, {"director", "backends", "up"}, {"director", "backends", "down"},
	} {
		if _, err := yarctl(t, args...); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("yarctl %s = %v, want its usage", strings.Join(args, " "), err)
		}
	}
}
