package protocol

import "testing"

func TestTabEscape(t *testing.T) {
	cases := []struct{ in, wire, back string }{
		{"plain", "plain", "plain"},
		{"a\tb", "a\x01tb", "a\tb"},
		{"a\r\nb", "a\x01r\x01nb", "a\r\nb"},
		{"a\x01b", "a\x011b", "a\x01b"},
		{"a\x00b", "ab", "ab"},
	}
	for _, tc := range cases {
		if got := TabEscape(tc.in); got != tc.wire {
			t.Errorf("TabEscape(%q) = %q, want %q", tc.in, got, tc.wire)
		}
		if got := TabUnescape(tc.wire); got != tc.back {
			t.Errorf("TabUnescape(%q) = %q, want %q", tc.wire, got, tc.back)
		}
	}
}
