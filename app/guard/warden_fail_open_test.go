package guard_test

import "testing"

// warden_service.fail_open reaches every login pod's config from the chart,
// false by default and true when set (#2475).
func TestWardenFailOpenReachesTheConfig(t *testing.T) {
	for _, tc := range []struct {
		name, values string
		want         bool
	}{
		{"default", "", false},
		{"set", "components:\n  warden:\n    service:\n      fail_open: true\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := renderWith(t, tc.values)
			if err != nil {
				t.Fatalf("helm template: %v\n%s", err, out)
			}
			if got := loadRendered(t, out).WardenService.FailOpen; got != tc.want {
				t.Errorf("FailOpen = %v, want %v", got, tc.want)
			}
		})
	}
}
