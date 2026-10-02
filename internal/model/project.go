package model

// Resolver assigns processes to git repositories (spec "Project resolution"). It keeps a
// per-directory cache across ticks, so the engine creates one and reuses it; it is not safe
// for concurrent use (only the refresh goroutine calls it).
//
// Owned by DEV-16; the body below is a placeholder.
type Resolver struct{}

// NewResolver returns a Resolver that stops walking at home (a repository rooted at home does
// not count) and, when roots is non-empty, only accepts repositories under one of roots.
func NewResolver(home string, roots []string) *Resolver {
	return &Resolver{}
}

// Resolve sets ProjectID on every element of procs in place (Build passes its own fresh
// slice), "" for the "other" group and for PID 0 pseudo-processes, and returns each project
// referenced by at least one process, once. It may read the filesystem through its cache.
//
// Placeholder: no projects, every ProjectID stays "".
func (r *Resolver) Resolve(procs []Process) []Project {
	return nil
}
