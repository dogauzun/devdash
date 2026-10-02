package docker

import "time"

// retryTicks is how many refresh ticks an endpoint that failed or a socket that was missing
// is left alone before the next request (spec "Failure modes": "retry every 10th tick").
const retryTicks = 10

// retrySlack is how early an attempt may come and still count as the retry. The engine calls
// Fetch on a fixed 5 s ticker grid, and each call starts after its beat by however long the
// goroutine took to wake; without slack, a retry beat that wakes a little earlier than the
// failing beat did would miss the holdoff and slip to the next beat. It is far above wake-up
// jitter and well below one 5 s beat, so the beat before the retry never counts.
const retrySlack = time.Second

// holdoff spaces out attempts at something that just failed: after wait(t), due reports
// false until 10 refresh ticks, less retrySlack, have passed since t. The zero holdoff is
// always due.
type holdoff struct {
	after time.Duration // retryTicks × the refresh tick
	until time.Time     // no attempt before this, less retrySlack; zero when nothing failed
}

// newHoldoff returns a holdoff of retryTicks refresh ticks of length tick.
func newHoldoff(tick time.Duration) holdoff { return holdoff{after: retryTicks * tick} }

// due reports whether an attempt may be made at now.
func (h *holdoff) due(now time.Time) bool { return !now.Before(h.until.Add(-retrySlack)) }

// wait records an attempt begun at start that failed: the next one is due once the holdoff
// has passed since start.
func (h *holdoff) wait(start time.Time) { h.until = start.Add(h.after) }
