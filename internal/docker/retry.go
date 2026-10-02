package docker

import "time"

// retryTicks is how many refresh ticks an endpoint that failed or a socket that was missing
// is left alone before the next request (spec "Failure modes": "retry every 10th tick").
const retryTicks = 10

// retrySlack is how early an attempt may come and still count as the retry, at most half a
// beat. The engine calls Fetch on a fixed beat grid (every 5 s), and each call starts after
// its beat by however long the goroutine took to wake; without slack, a retry beat that
// wakes a little earlier than the failing beat did would miss the holdoff and slip to the
// next beat. It is far above wake-up jitter, and because the holdoff is a whole number of
// beats the beat before the retry is a full beat early, so it never counts.
const retrySlack = time.Second

// holdoff spaces out attempts at something that just failed: after wait(t), due reports
// false until the holdoff, less its slack, has passed since t. The zero holdoff is always
// due.
type holdoff struct {
	after time.Duration // retryTicks refresh ticks, rounded up to whole beats
	slack time.Duration // retrySlack, at most half a beat; 0 without a beat
	until time.Time     // no attempt before this, less slack; zero when nothing failed
}

// newHoldoff returns a holdoff of retryTicks refresh ticks of length tick for attempts made
// on a grid of beat: rounded up to a whole number of beats, so the retry is the first beat
// at or after 10 ticks (with --tick 1.1s and a 5 s beat, 15 s rather than 11 s). A beat of
// 0 means no grid: exactly 10 ticks and no slack.
func newHoldoff(tick, beat time.Duration) holdoff {
	h := holdoff{after: retryTicks * tick}
	if beat > 0 {
		h.after = (h.after + beat - 1) / beat * beat
		h.slack = min(retrySlack, beat/2)
	}
	return h
}

// due reports whether an attempt may be made at now.
func (h *holdoff) due(now time.Time) bool { return !now.Before(h.until.Add(-h.slack)) }

// wait records an attempt begun at start that failed: the next one is due once the holdoff
// has passed since start.
func (h *holdoff) wait(start time.Time) { h.until = start.Add(h.after) }
