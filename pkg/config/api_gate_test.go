package config

import "testing"

func TestAnAdminAPIRefusesToRunOpen(t *testing.T) {
	for _, tc := range []struct {
		name     string
		token    string
		disabled bool
		nets     []string
		wantNets int
		bad      bool
	}{
		{name: "token", token: "t", wantNets: 0},
		{name: "token and networks", token: "t", nets: []string{"10.0.0.0/8", "192.168.0.0/16"}, wantNets: 2},
		{name: "explicitly open", disabled: true},
		{name: "no token", bad: true},
		{name: "token and auth_disabled", token: "t", disabled: true, bad: true},
		{name: "a malformed network", token: "t", nets: []string{"10.0.0.0/8", "nope"}, bad: true},
		{name: "only malformed networks", token: "t", nets: []string{"nope"}, bad: true},
	} {
		for _, gate := range []func() (string, int, error){
			func() (string, int, error) {
				tok, nets, err := DirectorAPIConfig{Token: tc.token, AuthDisabled: tc.disabled, AllowedNets: tc.nets}.Gate()
				return tok, len(nets), err
			},
			func() (string, int, error) {
				tok, nets, err := BackendAPIConfig{Token: tc.token, AuthDisabled: tc.disabled, AllowedNets: tc.nets}.Gate()
				return tok, len(nets), err
			},
		} {
			tok, n, err := gate()
			if tc.bad {
				if err == nil {
					t.Errorf("%s: started, want refused", tc.name)
				}
				continue
			}
			if err != nil || tok != tc.token || n != tc.wantNets {
				t.Errorf("%s: %q, %d nets, %v; want %q, %d", tc.name, tok, n, err, tc.token, tc.wantNets)
			}
		}
	}
}
