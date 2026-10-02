// Command devdash shows what is running on this machine, grouped by git repository.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/dogauzun/devdash/internal/collector"
)

// Set at build time with -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("devdash", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print one raw snapshot as JSON on stdout, timings on stderr")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	switch {
	case *asJSON && fs.NArg() == 0:
		return runJSON(stdout, stderr)
	case !*asJSON && fs.NArg() == 0:
		fmt.Fprintln(stderr, "TUI not implemented yet; try --json")
		return 2
	case !*asJSON && fs.NArg() == 1 && fs.Arg(0) == "version":
		fmt.Fprintf(stdout, "devdash %s (commit %s, built %s)\n", version, commit, date)
		return 0
	default:
		fmt.Fprintln(stderr, "usage: devdash [--json] | devdash version")
		return 2
	}
}

func runJSON(stdout, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	start := time.Now()
	res, err := collector.New().Collect(ctx)
	total := time.Since(start)
	if err != nil {
		fmt.Fprintln(stderr, "devdash:", err)
		return 1
	}

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		fmt.Fprintln(stderr, "devdash:", err)
		return 1
	}

	for _, name := range slices.Sorted(maps.Keys(res.Timings)) {
		fmt.Fprintf(stderr, "%-12s %v\n", name, res.Timings[name])
	}
	fmt.Fprintf(stderr, "%-12s %v\n", "total", total)
	return 0
}
