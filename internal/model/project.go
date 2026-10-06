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
	here  string             // devdash's own working directory, resolved; "" for none (SetHere)
	cache map[string]cached  // directory → repository found by its walk; only hits, across ticks
	tick  map[string]Project // directory → result for the current tick, misses included
	ticks bool               // NewTick was called: ticks are the caller's, and Resolve no longer starts one
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

// SetHere records devdash's own working directory (spec "Release 1.0", Here): from then on,
// Resolve marks the project of dir, found by steps 1-4 (no parent chain, no argv) under the
// same $HOME and roots rules as a process cwd, with Here. dir is resolved through symlinks
// once, here, like home and roots; "" or a relative dir (os.Getwd failed) means no Here.
func (r *Resolver) SetHere(dir string) {
	r.here = realPath(dir)
	if !filepath.IsAbs(r.here) {
		r.here = ""
	}
}

// NewTick starts a tick: every Resolve until the next NewTick looks each directory up at most
// once and reuses the result, a directory with no project included, so the engine's two
// Resolve calls per tick (collector.Options.InProject, then Build) walk once (DEV-207). Across
// ticks only directories that found a repository stay cached (dir). A Resolver whose caller
// never calls NewTick starts a tick on every Resolve.
func (r *Resolver) NewTick() {
	clear(r.tick)
	r.ticks = true
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
// slice), "" for the "other" group, for PID 0 pseudo-processes and for container-runtime
// processes (IsContainerRuntime), and returns each project referenced by at least one process,
// once, in order of first reference, the one of the SetHere directory with Here set. Each
// call is a tick of its own unless the caller marks ticks with NewTick.
//
// Steps 1–5 of the spec: the nearest repository above the process cwd, else above the cwd of
// its parent, grandparent and great-grandparent. Step 6, once every process has had steps
// 1–5: the first absolute argv path inside a project found so far. Step 7: "".
//
// A runtime process's cwd is the daemon's (dockerd started from a checkout, a systemd --user
// unit with a WorkingDirectory), not where its containers belong: it resolves to nothing, finds
// no project for step 6, and ends the parent chain of a process below it (a container's
// process under its shim), so a repository only runtime processes sit in is not a project.
func (r *Resolver) Resolve(procs []Process) []Project {
	if !r.ticks {
		clear(r.tick)
	}
	byPID := make(map[int]int, len(procs))
	skip := make([]bool, len(procs)) // PID 0 pseudo-processes and runtime processes
	for i, p := range procs {
		if p.PID != 0 {
			byPID[p.PID] = i
		}
		skip[i] = p.PID == 0 || IsContainerRuntime(p)
	}

	found := map[string]Project{}
	for i := range procs {
		procs[i].ProjectID = ""
		if skip[i] {
			continue
		}
		// Self, then up to three ancestors, stopping at a runtime process.
		for hop, j := 0, i; hop < 4 && !skip[j]; hop++ {
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
		if procs[i].ProjectID == "" && !skip[i] {
			procs[i].ProjectID = argvProject(procs[i].Argv, found)
		}
	}

	// Looked up on every call, through the cache like any cwd, so the Here project's branch
	// is the current one; devdash's own process has this cwd, so it is a tick-map hit.
	hereID := r.dir(r.here).ID
	var projects []Project
	for _, p := range procs {
		if p.ProjectID != "" {
			if proj, ok := found[p.ProjectID]; ok {
				proj.Here = hereID != "" && proj.ID == hereID
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
// A worktree's commondir never changes, and core.bare and core.worktree in the common config
// change only when a repository is reshaped (a submodule moved), so neither is checked: such a
// change shows on the next HEAD write or eviction.
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
// one), or a regular file with a gitdir: line (linked worktree when the gitdir has a commondir
// file naming the repository's common directory, otherwise a submodule or separate git dir,
// its own project). A symlinked .git, HEAD, commondir or config is refused, never followed.
// headFI is HEAD's lstat, nil if unreadable.
func repoAt(dir string) (p Project, head string, headFI fs.FileInfo, ok bool) {
	dotgit := filepath.Join(dir, ".git")
	fi, err := lstat(dotgit)
	if err != nil {
		return Project{}, "", nil, false
	}
	p = Project{ID: dir, Root: dir, Name: filepath.Base(dir), CommonDir: dotgit}
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
		p.CommonDir = gitdir
		if c := strings.TrimSpace(string(readRegular(filepath.Join(gitdir, "commondir")))); c != "" {
			if !filepath.IsAbs(c) {
				c = filepath.Join(gitdir, c)
			}
			p.CommonDir = filepath.Clean(c)
			p.Worktree = true
			p.MainRepo, p.Name = mainWorkTree(p.CommonDir)
		}
	default:
		return Project{}, "", nil, false
	}

	head = filepath.Join(gitdir, "HEAD")
	if hi, err := lstat(head); err == nil && hi.Mode().IsRegular() {
		headFI = hi
		s := strings.TrimSpace(string(readSmall(head)))
		// A reftable repository's HEAD file is the placeholder refs/heads/.invalid (no branch
		// may start with a dot); its real HEAD is in the tables, so the branch is unknown.
		// ponytail: no reftable parser; add one if reftable becomes common (git 3.0 default).
		if ref, isRef := strings.CutPrefix(s, "ref: "); isRef {
			if ref != "refs/heads/.invalid" {
				p.Branch = strings.TrimPrefix(ref, "refs/heads/")
			}
		} else if len(s) >= 7 {
			p.ShortSHA = s[:7]
		}
	}
	return p, head, headFI, true
}

// mainWorkTree returns the main work tree of the repository whose common directory is common
// and the repository's name. The work tree is core.worktree when set (git writes it for a
// submodule's git dir), else the directory above a non-bare common directory called .git, else
// "": a bare repository has none, and git records none for a --separate-git-dir. Without a
// work tree the name is the common directory's basename less a trailing .git (api.git → api),
// or its parent's basename when that leaves a dot-directory or nothing (shop2/.bare → shop2).
func mainWorkTree(common string) (main, name string) {
	bare, wt := coreConfig(common)
	switch {
	case wt != "":
		if !filepath.IsAbs(wt) {
			wt = filepath.Join(common, wt)
		}
		main = filepath.Clean(wt)
	case !bare && filepath.Base(common) == ".git":
		main = filepath.Dir(common)
	}
	if main != "" {
		return main, filepath.Base(main)
	}
	name = strings.TrimSuffix(filepath.Base(common), ".git")
	if name == "" || strings.HasPrefix(name, ".") {
		name = filepath.Base(filepath.Dir(common))
	}
	return "", name
}

// coreConfig reads core.bare and core.worktree from common/config.
// ponytail: only the plain `key = value` lines git writes itself in the first 4 KiB, where git
// init puts [core]; no includes, quoting or escapes. A full config parser if one ever matters.
func coreConfig(common string) (bare bool, worktree string) {
	section := ""
	for line := range strings.Lines(string(readRegular(filepath.Join(common, "config")))) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			section = strings.ToLower(strings.Trim(line, "[]"))
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || section != "core" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "bare":
			bare = strings.TrimSpace(v) == "true"
		case "worktree":
			worktree = strings.TrimSpace(v)
		}
	}
	return bare, worktree
}

// readRegular is readSmall of a path that lstat finds to be a regular file, else nil.
func readRegular(path string) []byte {
	if fi, err := lstat(path); err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	return readSmall(path)
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
