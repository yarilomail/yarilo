package main

import "testing"

// A capability line carries its parameters: the server answers
// "250-AUTH PLAIN LOGIN", and a check asking for "AUTH PLAIN" must find it.
// Keyed by the whole line, it found neither and failed a correct server (#1855).
func TestEHLOCapabilitiesCarryTheirParameters(t *testing.T) {
	caps := map[string]bool{}
	for _, line := range []string{"probe Hello", "AUTH PLAIN LOGIN", "XCLIENT ADDR PORT", "8BITMIME"} {
		addEHLOCap(caps, line)
	}
	for _, want := range []string{"AUTH", "AUTH PLAIN", "AUTH LOGIN", "XCLIENT", "XCLIENT ADDR", "8BITMIME"} {
		if !caps[want] {
			t.Errorf("capability %q not found in %v", want, caps)
		}
	}
	// And nothing is invented: a mechanism the server did not name is absent.
	for _, absent := range []string{"AUTH OAUTHBEARER", "STARTTLS"} {
		if caps[absent] {
			t.Errorf("capability %q was found though the server never named it", absent)
		}
	}
}
