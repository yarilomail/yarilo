package ftsservice

// testOpts fills what a test leaves unset with the chart's values: the package
// substitutes nothing, the config and these tests do.
func testOpts(o Options) Options {
	if o.Workers == 0 {
		o.Workers = 1
	}
	if o.CommitLimit == 0 {
		o.CommitLimit = 500
	}
	return o
}
