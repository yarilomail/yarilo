package director

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// An added backend that names no weight gets ring.DefaultVhosts; an explicit 0
// reaches the ring as 0, a backend that takes no traffic.
func TestAnAddedBackendKeepsTheWeightItNames(t *testing.T) {
	tests := []struct {
		name, body string
		want       int
	}{
		{"left out", `{"ip":"10.0.0.7","port":143,"tag":"a"}`, 100},
		{"zero", `{"ip":"10.0.0.7","port":143,"tag":"a","vhosts":0}`, 0},
		{"set", `{"ip":"10.0.0.7","port":143,"tag":"a","vhosts":40}`, 40},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewWithOptions(testOptions(Options{}))
			rec := httptest.NewRecorder()
			s.apiBackendAdd(rec, httptest.NewRequest("POST", "/api/director/backends", strings.NewReader(tc.body)))
			if rec.Code != 200 {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			if b := s.ring.GetBackend("10.0.0.7"); b == nil || b.Vhosts != tc.want {
				t.Errorf("backend = %+v, want vhosts %d", b, tc.want)
			}
		})
	}
}
