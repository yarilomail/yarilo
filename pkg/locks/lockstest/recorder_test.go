package lockstest

import (
	"context"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/locks"
)

func TestRecorderCountsByResourceAndType(t *testing.T) {
	r := &Recorder{}
	for _, e := range []struct {
		resource string
		t        locks.EventType
	}{{"subs:u", locks.EventSubscriptions}, {"subs:u", locks.EventSubscriptions}, {"subs:v", locks.EventSubscriptions}, {"mlist:u", locks.EventRole}} {
		_ = r.Emit(context.Background(), e.resource, e.t, "")
	}
	for _, tc := range []struct {
		resource string
		t        locks.EventType
		want     int
	}{
		{"subs:u", locks.EventSubscriptions, 2},
		{"subs:v", locks.EventSubscriptions, 1},
		{"mlist:u", locks.EventRole, 1},
		{"mlist:u", locks.EventACL, 0},
	} {
		if got := r.Count(tc.resource, tc.t); got != tc.want {
			t.Errorf("%s %s: %d, want %d", tc.resource, tc.t, got, tc.want)
		}
	}
}
