package model

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// maxCache caps the per-directory cache (spec: 4096 entries).
const maxCache = 4096

// lstat is os.Lstat; tests swap it to count filesystem calls.
var lstat = os.Lstat

// Resolver assigns processes to git repositories (spec "Project resolution"). It keeps a
// per-directory cache across ticks, so the engine creates one and reuses it; it is not safe
// for concurrent use (only the refresh goroutine calls it).
type Resolver struct {
	home  string
	roots []string
	cache map[string]cached  // directory → repository found by its walk; only hits, across ticks
	tick  map[string]Project // directory → result for the current Resolve call, misses included
}

// cached is a walk result plus what validates it: the HEAD file and its lstat when read.
type cached struct {
	p    Project
	head string
	fi   fs.FileInfo
}

// NewResolver returns a Resolver that stops walking at home (a repository rooted at home does
// not count) and, when roots is non-empty, only accepts repositories under one of roots.
// home and roots are resolved through symlinks once, here, because the cwds the collectors
// report are the kernel's resolved paths.
func NewResolver(home string, roots []string) *Resolver {
	r := &Resolver{home: realPath(home), cache: map[string]cached{}, tick: map[string]Project{}}
	for _, root := range roots {
		if root != "" {
			r.roots = append(r.roots, realPath(root))
		}
	}
	return r
}

func realPath(p string) string {
	if p == "" {
		return ""
	}
	if rp, err := filepath.EvalSymlinks(p); err == nil {
		return rp
	}
	return filepath.Clean(p)
}

// Resolve sets ProjectID on every element of procs in place (Build passes its own fresh
// slice), "" for the "other" group and for PID 0 pseudo-processes, and returns each project
// referenced by at least one process, once, in order of first reference.
//
// Steps 1–5 of the spec: the nearest repository above the process cwd, else above the cwd of
// its parent, grandparent and great-grandparent. Step 6, once every process has had steps
// 1–5: the first absolute argv path inside a project found so far. Step 7: "".
func (r *Resolver) Resolve(procs []Process) []Project {
	clear(r.tick)
	byPID := make(map[int]int, len(procs))
	for i, p := range procs {
		if p.PID != 0 {
			byPID[p.PID] = i
		}
	}

	found := map[string]Project{}
	for i := range procs {
		procs[i].ProjectID = ""
		if procs[i].PID == 0 {
			continue
		}
		// Self, then up to three ancestors.
		for hop, j := 0, i; hop < 4; hop++ {
			if p := r.dir(cwdOf(procs[j])); p.ID != "" {
				procs[i].ProjectID = p.ID
				found[p.ID] = p
				break
			}
			k, ok := byPID[procs[j].PPID]
			if !ok || k == j {
				break
			}
			j = k
		}
	}

	for i := range procs {
		if procs[i].ProjectID == "" && procs[i].PID != 0 {
			procs[i].ProjectID = argvProject(procs[i].Argv, found)
		}
	}

	var projects []Project
	for _, p := range procs {
		if p.ProjectID != "" {
			if proj, ok := found[p.ProjectID]; ok {
				projects = append(projects, proj)
				delete(found, p.ProjectID)
			}
		}
	}
	return projects
}

// cwdOf is the directory to walk from, "" when the cwd is unknown or not an absolute path.
func cwdOf(p Process) string {
	if p.Unknown&FieldCwd != 0 || !filepath.IsAbs(p.Cwd) {
		return ""
	}
	return filepath.Clean(p.Cwd)
}

// argvProject returns the project containing the first absolute argv path that lies inside
// one of found (the innermost when projects nest), or "". Nothing is stat'ed.
func argvProject(argv []string, found map[string]Project) string {
	for _, a := range argv {
		if !filepath.IsAbs(a) {
			continue
		}
		a = filepath.Clean(a)
		best := ""
		for id := range found {
			if under(a, id) && len(id) > len(best) {
				best = id
			}
		}
		if best != "" {
			return best
		}
	}
	return ""
}

// under reports whether path is dir or lies below it.
func under(path, dir string) bool {
	return path == dir || dir == "/" || strings.HasPrefix(path, dir+"/")
}

// dir returns the project for directory d (ID "" for none), at most once per tick per d.
//
// Cache hit: one lstat of the repository's HEAD file (.git/HEAD, or <gitdir>/HEAD for a
// worktree), compared with the lstat recorded when HEAD was read: same file (inode), size and
// mtime. git rewrites HEAD by renaming HEAD.lock over it on every branch switch (and on every
// commit while detached), so every write is a new inode even when a coarse-mtime filesystem
// gives it the old timestamp; a changed file, or HEAD vanishing with the repository, is a miss
// and the walk runs again.
// The .git entry's own mtime is not checked: for a main repository it changes on every index
// write, which would make most hits misses, and a worktree's .git file never changes.
// Known ceiling: a repository created between d and a cached root (or above a directory that
// had none) is seen only after eviction; directories with no project are not cached across
// ticks, so `git init` above a running process is seen on the next tick.
func (r *Resolver) dir(d string) Project {
	if d == "" {
		return Project{}
	}
	if p, ok := r.tick[d]; ok {
		return p
	}
	if c, ok := r.cache[d]; ok {
		if fi, err := lstat(c.head); err == nil && os.SameFile(fi, c.fi) && fi.Size() == c.fi.Size() && fi.ModTime().Equal(c.fi.ModTime()) {
			r.tick[d] = c.p
			return c.p
		}
		delete(r.cache, d)
	}
	p, head, fi := r.walk(d)
	if p.ID != "" && fi != nil {
		if len(r.cache) >= maxCache {
			// Evict one arbitrary entry (Go map order). Correct for any victim: an evicted
			// directory costs one walk the next time it is seen.
			for k := range r.cache {
				delete(r.cache, k)
				break
			}
		}
		r.cache[d] = cached{p, head, fi}
	}
	r.tick[d] = p
	return p
}

// walk goes up from d to the nearest .git entry (spec steps 2–4). It stops at the filesystem
// root or at home without checking home itself, so a repository rooted at home never counts.
// With roots, the walk stops before the first directory outside every root, without touching
// it: a project root must lie under a root, and so does every directory between it and the
// cwd. A cwd outside the roots (or above one) costs no filesystem call, so --roots keeps a
// slow or hung mount out of the refresh path.
func (r *Resolver) walk(d string) (Project, string, fs.FileInfo) {
	for ; d != r.home && r.inRoots(d); d = filepath.Dir(d) {
		if p, head, fi, ok := repoAt(d); ok {
			return p, head, fi
		}
		if d == filepath.Dir(d) {
			break
		}
	}
	return Project{}, "", nil
}

func (r *Resolver) inRoots(d string) bool {
	if len(r.roots) == 0 {
		return true
	}
	for _, root := range r.roots {
		if under(d, root) {
			return true
		}
	}
	return false
}

// repoAt reports whether dir holds a .git entry: a directory (main repository, or a nested
// one), or a regular file with a gitdir: line (linked worktree when the gitdir is
// <common>/worktrees/<name>, otherwise a submodule or separate git dir, its own project).
// A symlinked .git, or HEAD, is refused, never followed. headFI is HEAD's lstat, nil if unreadable.
func repoAt(dir string) (p Project, head string, headFI fs.FileInfo, ok bool) {
	dotgit := filepath.Join(dir, ".git")
	fi, err := lstat(dotgit)
	if err != nil {
		return
	}
	p = Project{ID: dir, Root: dir, Name: filepath.Base(dir)}
	gitdir := dotgit
	switch {
	case fi.IsDir():
	case fi.Mode().IsRegular():
		line, _, _ := strings.Cut(string(readSmall(dotgit)), "\n")
		g, found := strings.CutPrefix(strings.TrimSpace(line), "gitdir: ")
		if !found || g == "" {
			return Project{}, "", nil, false
		}
		if !filepath.IsAbs(g) {
			g = filepath.Join(dir, g)
		}
		gitdir = filepath.Clean(g)
		if filepath.Base(filepath.Dir(gitdir)) == "worktrees" {
			main := filepath.Dir(filepath.Dir(gitdir)) // <common> of <common>/worktrees/<name>
			if filepath.Base(main) == ".git" {
				main = filepath.Dir(main) // non-bare: the work tree above .git
			}
			p.Worktree, p.MainRepo, p.Name = true, main, filepath.Base(main)
		}
	default:
		return Project{}, "", nil, false
	}

	head = filepath.Join(gitdir, "HEAD")
	if hi, err := lstat(head); err == nil && hi.Mode().IsRegular() {
		headFI = hi
		s := strings.TrimSpace(string(readSmall(head)))
		if ref, isRef := strings.CutPrefix(s, "ref: "); isRef {
			p.Branch = strings.TrimPrefix(ref, "refs/heads/")
		} else if len(s) >= 7 {
			p.ShortSHA = s[:7]
		}
	}
	return p, head, headFI, true
}

// readSmall reads at most 4 KiB of a file already checked to be regular by lstat; .git files
// and HEAD are a line each, and a stray large file must not be read whole every tick.
func readSmall(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	b, _ := io.ReadAll(io.LimitReader(f, 4096))
	return b
}
