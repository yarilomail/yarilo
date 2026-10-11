package lmtp

// testOpts fills what a test leaves unset with the chart's values: the package
// substitutes nothing, the config and these tests do.
func testOpts(o Options) Options {
	if o.QuotaPolicy.StoragePercentage == 0 {
		o.QuotaPolicy.StoragePercentage = 100
	}
	if o.QuotaPolicy.MessagePercentage == 0 {
		o.QuotaPolicy.MessagePercentage = 100
	}
	return o
}
