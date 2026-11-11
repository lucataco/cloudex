package node

import (
	"math/rand/v2"
	"time"
)

// Backoff is how long to wait before reconnect attempt n (0 is the first
// retry after a drop). From the spikes (N4): retry fast for the first second,
// because a deploy or a restart of the Worker is usually over by then, then
// double to a 5 s cap, with jitter so that many agents don't arrive together.
//
//	n = 0:     0-250 ms
//	n = 1..3:  250 ms each (the first second)
//	n = 4..:   500 ms, 1 s, 2 s, 4 s, then 5 s, each 50-100% of that
func Backoff(n int, r *rand.Rand) time.Duration {
	f := rand.Float64
	if r != nil {
		f = r.Float64
	}
	switch {
	case n <= 0:
		return time.Duration(f() * float64(250*time.Millisecond))
	case n <= 3:
		return 250 * time.Millisecond
	}
	d := 500 * time.Millisecond << min(n-4, 4)
	d = min(d, BackoffCap)
	return time.Duration(float64(d) * (0.5 + f()*0.5))
}

// BackoffCap is the longest normal wait between attempts.
const BackoffCap = 5 * time.Second

// RefusedWait is how long the agent waits after the Worker refuses it (a
// revoked or unknown key, a skewed clock, another protocol), when trying again
// at once wouldn't help. Each attempt costs the Worker a D1 read.
const RefusedWait = 60 * time.Second

// ReplacedWait is how long the agent waits after another connection with the
// same key replaced its own: two agents sharing one state directory would
// otherwise take turns forever.
const ReplacedWait = 30 * time.Second
