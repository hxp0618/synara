package workertiming

import "time"

const DefaultLeaseTTL = 30 * time.Second

// LeaseRenewInterval keeps every managed agentd on the same timing contract as
// the authoritative Control Plane lease TTL. The one-second floor avoids a hot
// renewal loop, while the final guard keeps unusually small test TTLs safe.
func LeaseRenewInterval(leaseTTL time.Duration) time.Duration {
	if leaseTTL <= 0 {
		leaseTTL = DefaultLeaseTTL
	}
	interval := leaseTTL / 3
	if interval < time.Second {
		interval = time.Second
	}
	if interval >= leaseTTL {
		interval = leaseTTL / 2
	}
	if interval <= 0 {
		return time.Millisecond
	}
	return interval
}
