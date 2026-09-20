package backend

import (
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/filelock"
)

// Zero cannot mean both "not set" and "never take a dotlock over": a config
// that wrote 0 to disable the override used to get the reference's 180s (#1831).
func TestStaleTimeoutOf(t *testing.T) {
	tests := []struct {
		name string
		secs int
		want time.Duration
	}{
		{name: "unset keeps the reference's", secs: 0, want: filelock.DefaultStaleTimeout},
		{name: "a duration is taken as seconds", secs: 45, want: 45 * time.Second},
		{name: "-1 never takes one over", secs: -1, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := staleTimeoutOf(config.StorageConfig{LockStaleTimeout: tc.secs}); got != tc.want {
				t.Errorf("staleTimeoutOf(%d) = %s, want %s", tc.secs, got, tc.want)
			}
		})
	}
}
