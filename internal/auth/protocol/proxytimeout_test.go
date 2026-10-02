package protocol

import (
	"testing"
	"time"
)

func TestParseProxyTimeout(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{in: "", want: 0},
		{in: "0", want: 0},
		{in: "30", want: 30 * time.Second},
		{in: "30s", want: 30 * time.Second},
		{in: "30 S", want: 30 * time.Second},
		{in: "30secs", want: 30 * time.Second},
		{in: "500ms", want: 500 * time.Millisecond},
		{in: "500 msecs", want: 500 * time.Millisecond},
		{in: "2m", want: 2 * time.Minute},
		{in: "2mins", want: 2 * time.Minute},
		{in: "1h", want: time.Hour},
		{in: "1d", want: 24 * time.Hour},
		{in: "1w", want: 7 * 24 * time.Hour},
		{in: "4294967295ms", want: 4294967295 * time.Millisecond},
		{in: "4294967296ms", bad: true},
		{in: "4294968", bad: true},
		{in: "50d", bad: true},
		{in: "-1", bad: true},
		{in: "abc", bad: true},
		{in: "30x", bad: true},
		{in: "30sx", bad: true},
		{in: " 30", bad: true},
	} {
		got, err := ParseProxyTimeout(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("%q: got %v, want an error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%q: got %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
}
