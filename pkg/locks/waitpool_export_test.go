package locks

import "time"

// AgeWaitPool makes every kept waiting connection look idle for d, so a row
// about the idle timeout does not have to wait it out.
func AgeWaitPool(c *Client, d time.Duration) {
	c.waitMu.Lock()
	defer c.waitMu.Unlock()
	for _, slot := range c.waitFree {
		slot.idleSince = slot.idleSince.Add(-d)
	}
}
