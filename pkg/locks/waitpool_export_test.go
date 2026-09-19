package locks

import "time"

// SetWatchStopDelay widens the window between a wait's answer and the watcher
// letting the connection go, so the order of the two is observable in a row
// rather than argued from the code.
func SetWatchStopDelay(d time.Duration) { watchStopDelay = d }

// AgeWaitPool makes every kept waiting connection look idle for d, so a row
// about the idle timeout does not have to wait it out.
func AgeWaitPool(c *Client, d time.Duration) {
	c.waitMu.Lock()
	defer c.waitMu.Unlock()
	for _, slot := range c.waitFree {
		slot.idleSince = slot.idleSince.Add(-d)
	}
}
