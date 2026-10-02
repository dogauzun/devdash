package collector

import (
	"context"
	"errors"
	"slices"
	"sync"
)

// Fake is a Collector for tests: each Collect plays the next Step, and the last Step repeats
// once the script runs out. It is safe for concurrent use.
type Fake struct {
	Steps []Step

	mu    sync.Mutex
	next  int
	calls []Options
}

// Step is one scripted Collect outcome.
type Step struct {
	Result Result
	Err    error
	Block  bool // wait until ctx is done, then return ctx.Err()
}

// Collect implements Collector. It records o and plays the next Step as scripted, whatever o
// asks for.
func (f *Fake) Collect(ctx context.Context, o Options) (Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, o)
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

// Calls returns the Options of every Collect so far, in order.
func (f *Fake) Calls() []Options {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}
