package login

import "time"

// testOpts fills what a test leaves unset with the chart's values: the package
// substitutes nothing, the config and these tests do.
func testOpts(o Options) Options {
	if o.AuthMaxAttempts == 0 {
		o.AuthMaxAttempts = 3
	}
	if o.WardenConns == 0 {
		o.WardenConns = 4
	}
	if o.SieveMaxInvalidCmds == 0 {
		o.SieveMaxInvalidCmds = 3
	}
	if o.LookupHoldMax == 0 {
		o.LookupHoldMax = 20
	}
	if o.LookupHoldBackoff == 0 {
		o.LookupHoldBackoff = 150 * time.Millisecond
	}
	if o.SessionSyncInterval == 0 {
		o.SessionSyncInterval = 30 * time.Second
	}
	if o.ProxyTimeout == 0 {
		o.ProxyTimeout = 30 * time.Second
	}
	if o.HAProxyTimeout == 0 {
		o.HAProxyTimeout = 3 * time.Second
	}
	return o
}
