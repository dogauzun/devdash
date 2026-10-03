package main

import (
	"context"
	"fmt"
	"io"

	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/freeport"
)

// probe checks whether this user can bind a port; tests replace it.
var probe freeport.Prober = freeport.Probe

// runFree answers "which port can I use instead" from one snapshot (no CPU, so one sample):
// the first port from `from` to freeport.Last(from) that nothing in the snapshot holds and
// that probe can bind, on its own line with exit 0, so `PORT=$(devdash free 3000)` works;
// nothing and exit 1 when none is. A snapshot or probe that fails is exit 5 with nothing on
// stdout: 1 only answers the question asked.
func runFree(ctx context.Context, o engine.Options, from uint16, stdout, stderr io.Writer) int {
	s, err := engine.Snapshot(ctx, o)
	if err != nil {
		fmt.Fprintln(stderr, "devdash:", err)
		return exitFailed
	}
	port, ok, err := freeport.Find(s, from, probe)
	switch {
	case err != nil:
		fmt.Fprintln(stderr, "devdash:", err)
		return exitFailed
	case !ok:
		return 1
	}
	return write(stdout, stderr, fmt.Sprintf("%d\n", port), 0)
}
