package workertiming

import (
	"testing"
	"time"
)

func TestLeaseRenewIntervalTracksAuthoritativeTTL(t *testing.T) {
	for _, test := range []struct {
		name string
		ttl  time.Duration
		want time.Duration
	}{
		{name: "default", ttl: 0, want: 10 * time.Second},
		{name: "production", ttl: 30 * time.Second, want: 10 * time.Second},
		{name: "acceptance", ttl: 6 * time.Second, want: 2 * time.Second},
		{name: "short", ttl: 2 * time.Second, want: time.Second},
		{name: "subsecond", ttl: 500 * time.Millisecond, want: 250 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := LeaseRenewInterval(test.ttl); got != test.want {
				t.Fatalf("LeaseRenewInterval(%s) = %s, want %s", test.ttl, got, test.want)
			}
		})
	}
}
