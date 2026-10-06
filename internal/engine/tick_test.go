package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dogauzun/devdash/internal/collector"
	"github.com/dogauzun/devdash/internal/model"
)

// inProjectCollector calls InProject as the collector does over the process limit, then runs
// after, and returns procs.
type inProjectCollector struct {
	procs []collector.Process
	after func()
}

func (c inProjectCollector) Collect(_ context.Context, o collector.Options) (collector.Result, error) {
	o.InProject(c.procs)
	c.after()
	return collector.Result{Processes: c.procs}, nil
}

// TestTickSharesResolve: InProject and Build resolve in the same tick of the Resolver
// (DEV-207), so Build reuses InProject's walks: a repository created between the two is not
// seen by Build, and is seen on the next tick, which starts a new one.
func TestTickSharesResolve(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitInit := func() {
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := inProjectCollector{procs: []collector.Process{{PID: 10, PPID: 1, Name: "p", Cwd: dir}}, after: gitInit}
	e := New(Options{Collector: c, Resolver: model.NewResolver("", nil), LookupUser: func(int) string { return "u" }})
	for tick, want := range []string{"", dir} {
		raw, err := e.collect(context.Background(), collector.Options{InProject: e.inProject})
		if err != nil {
			t.Fatal(err)
		}
		if got := e.build(raw, model.Snapshot{}, dockerResult{}).Processes[0].ProjectID; got != want {
			t.Errorf("tick %d: ProjectID %q, want %q", tick, got, want)
		}
	}
}
