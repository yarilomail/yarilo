package lmtplogin

import "time"

// testOpts fills what a test leaves unset with the chart's values: the package
// substitutes nothing, the config and these tests do.
func testOpts(o Options) Options {
	if o.ProxyTimeout == 0 {
		o.ProxyTimeout = 125 * time.Second
	}
	if o.HAProxyTimeout == 0 {
		o.HAProxyTimeout = 3 * time.Second
	}
	return o
}
