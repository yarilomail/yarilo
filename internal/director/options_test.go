package director

import "time"

// testOptions fills what a test leaves unset with the chart's values: the
// package substitutes nothing, the config and these tests do.
func testOptions(o Options) Options {
	fill := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	fillInt := func(n *int, v int) {
		if *n == 0 {
			*n = v
		}
	}
	fill(&o.UserExpire, 900*time.Second)
	fill(&o.PingInterval, 30*time.Second)
	fill(&o.PingTimeout, 10*time.Second)
	fill(&o.WriteTimeout, 10*time.Second)
	fill(&o.AntiEntropyInterval, 3*time.Second)
	fill(&o.SeedPollInterval, 2*time.Second)
	fill(&o.SeedPollIdleInterval, 2*time.Second)
	fill(&o.BackendExpire, 30*time.Second)
	fillInt(&o.UnreachableReporters, 2)
	fill(&o.UnreachableWindow, 5*time.Second)
	fill(&o.TombstoneTTL, 600*time.Second)
	fill(&o.DomainRebalanceInterval, time.Minute)
	fill(&o.DomainRebalanceCooldown, 10*time.Minute)
	fill(&o.DomainExpire, 900*time.Second)
	fill(&o.UserKickDelay, 2*time.Second)
	fillInt(&o.MaxParallelKicks, 100)
	fillInt(&o.MaxParallelMoves, 5)
	fill(&o.FlushProgramTimeout, 10*time.Second)
	fill(&o.UserKillTimeout, 15*time.Second)
	fill(&o.UserKillConfirmGrace, time.Second)
	return o
}
