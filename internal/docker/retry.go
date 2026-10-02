package docker

import "time"

// retryTicks is how many refresh ticks an endpoint that failed or a socket that was missing
// is left alone before the next request (spec "Failure modes": "retry every 10th tick").
const retryTicks = 10

// holdoff spaces out attempts at something that just failed: after wait(t), due reports
// false until 10 refresh ticks have passed since t. The zero holdoff is always due.
type holdoff struct {
	after time.Duration // retryTicks × the refresh tick
	until time.Time     // no attempt before this; zero when nothing failed
}

// newHoldoff returns a holdoff of retryTicks refresh ticks of length tick.
func newHoldoff(tick time.Duration) holdoff { return holdoff{after: retryTicks * tick} }

// due reports whether an attempt may be made at now.
func (h *holdoff) due(now time.Time) bool { return !now.Before(h.until) }

// wait records an attempt begun at start that failed: the next one is due once the holdoff
// has passed since start.
func (h *holdoff) wait(start time.Time) { h.until = start.Add(h.after) }
