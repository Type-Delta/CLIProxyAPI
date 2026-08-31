package collector

import "time"

type circuit struct {
	threshold    int
	failures     int
	open         bool
	permanent    bool
	retryBackoff time.Duration
}

func newCircuit(threshold int) circuit {
	return circuit{threshold: threshold, retryBackoff: 30 * time.Second}
}

func (c *circuit) failed(permanent bool) (opened bool) {
	c.failures++
	if permanent || c.failures >= c.threshold {
		c.open = true
		c.permanent = permanent
		return true
	}
	return false
}

func (c *circuit) succeeded() {
	c.failures = 0
	c.open = false
	c.permanent = false
	c.retryBackoff = 30 * time.Second
}

func (c *circuit) nextBackoff() time.Duration {
	delay := c.retryBackoff
	c.retryBackoff *= 2
	if c.retryBackoff > 5*time.Minute {
		c.retryBackoff = 5 * time.Minute
	}
	return delay
}
