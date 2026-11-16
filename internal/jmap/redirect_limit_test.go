package jmap

import (
	"encoding/json"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/jmapcore"
)

// sieve_max_redirects 0 is advertised as 0, not as null (no limit), and a
// script with redirect is refused under it (#2529).
func TestSieveRedirectLimit(t *testing.T) {
	const script = `redirect "a@example.org";`
	cases := []struct {
		max       int
		wantCap   string
		wantError bool
	}{
		{0, "0", true},
		{32, "32", false},
	}
	for _, c := range cases {
		o := &SieveScripts{MaxRedirects: c.max}
		raw, _ := json.Marshal(o.session().Account)
		var got map[string]json.RawMessage
		_ = json.Unmarshal(raw, &got)
		if string(got["maxNumberRedirects"]) != c.wantCap {
			t.Errorf("max %d: maxNumberRedirects %s, want %s", c.max, got["maxNumberRedirects"], c.wantCap)
		}
		s := &Server{opts: Options{Sieve: o}}
		serr := s.checkSieveContent([]byte(script))
		if (serr != nil) != c.wantError || (serr != nil && serr.Type != jmapcore.SetErrInvalidSieve) {
			t.Errorf("max %d: set error %+v, want error %v", c.max, serr, c.wantError)
		}
	}
}
