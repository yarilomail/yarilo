//go:build flatcurve

package flatcurve

// testOpts fills what a test leaves unset with the chart's values: the package
// substitutes nothing, the config and these tests do.
func testOpts(o Options) Options {
	if o.CommitLimit == 0 {
		o.CommitLimit = 500
	}
	if o.MinTermSize == 0 {
		o.MinTermSize = 2
	}
	if o.RotateCount == 0 {
		o.RotateCount = 5000
	}
	return o
}
