package collector

import (
	"context"
	"errors"
	"sync"
)

// Fake is a Collector for tests: each Collect plays the next Step, and the last Step repeats
// once the script runs out. It is safe for concurrent use.
type Fake struct {
	Steps []Step

	mu   sync.Mutex
	next int
}

// Step is one scripted Collect outcome.
type Step struct {
	Result Result
	Err    error
	Block  bool // wait until ctx is done, then return ctx.Err()
}

// Collect implements Collector.
func (f *Fake) Collect(ctx context.Context) (Result, error) {
	f.mu.Lock()
	if len(f.Steps) == 0 {
		f.mu.Unlock()
		return Result{}, errors.New("collector.Fake: no steps")
	}
	s := f.Steps[min(f.next, len(f.Steps)-1)]
	f.next++
	f.mu.Unlock()

	if s.Block {
		<-ctx.Done()
		return Result{}, ctx.Err()
	}
	return s.Result, s.Err
}
