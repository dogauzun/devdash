package model

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// gitBase holds repositories built with the real git binary in TestMain; "" when git is absent.
//
//	shop/                 main repository on main, with sub/deep/
//	shop/vendor/lib/      nested repository on dev
//	shop-wt/              linked worktree of shop on feat/cart
//	detached/             repository with a detached HEAD at detachedSHA
var gitBase, detachedSHA string

func TestMain(m *testing.M) {
	code := func() int {
		if _, err := exec.LookPath("git"); err == nil {
			dir, err := os.MkdirTemp("", "devdash-project")
			if err != nil {
				panic(err)
			}
			defer func() { _ = os.RemoveAll(dir) }()
			if err := buildGitFixtures(dir); err != nil {
				panic(err)
			}
		}
		return m.Run()
	}()
	os.Exit(code)
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func buildGitFixtures(dir string) error {
	base, err := filepath.EvalSymlinks(dir) // macOS: /var → /private/var, as the kernel reports cwds
	if err != nil {
		return err
	}
	for _, d := range []string{"shop/sub/deep", "shop/vendor/lib", "detached"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			return err
		}
	}
	shop, lib, det := filepath.Join(base, "shop"), filepath.Join(base, "shop/vendor/lib"), filepath.Join(base, "detached")
	for _, step := range [][]string{
		{shop, "init", "-q", "-b", "main"},
		{shop, "commit", "-q", "--allow-empty", "-m", "init"},
		{shop, "worktree", "add", "-q", "-b", "feat/cart", filepath.Join(base, "shop-wt")},
		{lib, "init", "-q", "-b", "dev"},
		{det, "init", "-q", "-b", "main"},
		{det, "commit", "-q", "--allow-empty", "-m", "init"},
		{det, "checkout", "-q", "--detach"},
	} {
		if _, err := git(step[0], step[1:]...); err != nil {
			return err
		}
	}
	if detachedSHA, err = git(det, "rev-parse", "HEAD"); err != nil {
		return err
	}
	gitBase = base
	return nil
}

func needGit(t *testing.T) string {
	t.Helper()
	if gitBase == "" {
		t.Skip("git not installed")
	}
	return gitBase
}

// tmp is a resolved temporary directory.
func tmp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// mkfile writes content to base/rel, creating parent directories.
func mkfile(t *testing.T, base, rel, content string) {
	t.Helper()
	p := filepath.Join(base, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mkrepo hand-builds base/rel/.git/HEAD pointing at branch.
func mkrepo(t *testing.T, base, rel, branch string) string {
	t.Helper()
	mkfile(t, base, filepath.Join(rel, ".git/HEAD"), "ref: refs/heads/"+branch+"\n")
	return filepath.Join(base, rel)
}

func mkdir(t *testing.T, base, rel string) string {
	t.Helper()
	p := filepath.Join(base, rel)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func inDir(pid int, cwd string) Process {
	return Process{PID: pid, PPID: 1, Name: "p", Cwd: cwd}
}

func project(root, branch string) Project {
	return Project{ID: root, Root: root, Name: filepath.Base(root), Branch: branch, CommonDir: filepath.Join(root, ".git")}
}

func resolveOne(r *Resolver, p Process) (string, []Project) {
	procs := []Process{p}
	projects := r.Resolve(procs)
	return procs[0].ProjectID, projects
}

func TestResolveGit(t *testing.T) {
	base := needGit(t)
	shop := filepath.Join(base, "shop")
	wt := filepath.Join(base, "shop-wt")
	lib := filepath.Join(shop, "vendor/lib")
	det := filepath.Join(base, "detached")
	for _, tc := range []struct {
		name, cwd string
		want      Project
	}{
		{"main repo root", shop, project(shop, "main")},
		{"main repo subdirectory", filepath.Join(shop, "sub/deep"), project(shop, "main")},
		{"nested repo", lib, project(lib, "dev")},
		{"nested repo parent", filepath.Join(shop, "vendor"), project(shop, "main")},
		{"linked worktree", wt, Project{ID: wt, Root: wt, Name: "shop", Branch: "feat/cart", Worktree: true, MainRepo: shop, CommonDir: filepath.Join(shop, ".git")}},
		{"detached HEAD", det, Project{ID: det, Root: det, Name: "detached", ShortSHA: detachedSHA[:7], CommonDir: filepath.Join(det, ".git")}},
		{"outside any repo", base, Project{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, projects := resolveOne(NewResolver("", nil), inDir(10, tc.cwd))
			if id != tc.want.ID {
				t.Errorf("ProjectID %q, want %q", id, tc.want.ID)
			}
			var want []Project
			if tc.want.ID != "" {
				want = []Project{tc.want}
			}
			if !slices.Equal(projects, want) {
				t.Errorf("projects %+v, want %+v", projects, want)
			}
		})
	}
}

// TestResolveWorktreeLayoutsGit: a linked worktree is named after its repository, never after
// its git directory, MainRepo is the main work tree or "" when git's files name none, and a
// worktree is "this repo, other worktree" from any other worktree of the repository (DEV-151).
func TestResolveWorktreeLayoutsGit(t *testing.T) {
	needGit(t)
	base := tmp(t)
	run := func(dir string, args ...string) {
		t.Helper()
		if _, err := git(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	seed, web, super := mkdir(t, base, "seed"), mkdir(t, base, "web"), mkdir(t, base, "super")
	mkdir(t, base, "gitdirs")
	shop2, apiGit, sub := filepath.Join(base, "shop2"), filepath.Join(base, "api.git"), filepath.Join(super, "sub")
	run(seed, "init", "-q", "-b", "main")
	run(seed, "commit", "-q", "--allow-empty", "-m", "init")
	// A: a bare clone in shop2/.bare, named by the file shop2/.git, with worktrees inside shop2.
	run(base, "clone", "-q", "--bare", seed, filepath.Join(shop2, ".bare"))
	mkfile(t, shop2, ".git", "gitdir: ./.bare\n")
	run(shop2, "worktree", "add", "-q", "main-wt", "main")
	run(shop2, "worktree", "add", "-q", "-b", "feat2", "feat-wt")
	// B: a separate git dir, with a worktree.
	run(web, "init", "-q", "-b", "main", "--separate-git-dir", filepath.Join(base, "gitdirs/web.git"))
	run(web, "commit", "-q", "--allow-empty", "-m", "init")
	run(web, "worktree", "add", "-q", "-b", "wt", filepath.Join(base, "web-wt"))
	// C: a plain bare clone, with two worktrees.
	run(base, "clone", "-q", "--bare", seed, apiGit)
	run(apiGit, "worktree", "add", "-q", filepath.Join(base, "api-main"), "main")
	run(apiGit, "worktree", "add", "-q", "-b", "topic", filepath.Join(base, "api-topic"))
	// D: a worktree added from inside a submodule.
	run(super, "init", "-q", "-b", "main")
	run(super, "-c", "protocol.file.allow=always", "submodule", "add", "-q", seed, "sub")
	run(sub, "worktree", "add", "-q", "-b", "subwt", filepath.Join(base, "sub-wt"))

	wt := func(dir, name, branch, main string) Project {
		return Project{ID: dir, Root: dir, Name: name, Branch: branch, Worktree: true, MainRepo: main}
	}
	featWT, mainWT := filepath.Join(shop2, "feat-wt"), filepath.Join(shop2, "main-wt")
	for _, tc := range []struct {
		name, cwd, here string
		want            Project
		loc             string
	}{
		{"A: bare in .bare, from a sibling", featWT, mainWT, wt(featWT, "shop2", "feat2", ""), "this repo, other worktree"},
		{"A: bare in .bare, from the .git file's directory", featWT, shop2, wt(featWT, "shop2", "feat2", ""), "this repo, other worktree"},
		{"B: separate git dir, from the main work tree", filepath.Join(base, "web-wt"), web, wt(filepath.Join(base, "web-wt"), "web", "wt", ""), "this repo, other worktree"},
		{"C: plain bare clone, from a sibling", filepath.Join(base, "api-topic"), filepath.Join(base, "api-main"), wt(filepath.Join(base, "api-topic"), "api", "topic", ""), "this repo, other worktree"},
		{"D: submodule, from the submodule", filepath.Join(base, "sub-wt"), sub, wt(filepath.Join(base, "sub-wt"), "sub", "subwt", sub), "this repo, other worktree"},
		{"two bare repositories", featWT, filepath.Join(base, "api-main"), wt(featWT, "shop2", "feat2", ""), ""},
		{"submodule and its superproject", filepath.Join(base, "sub-wt"), super, wt(filepath.Join(base, "sub-wt"), "sub", "subwt", sub), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewResolver("", nil)
			r.SetHere(tc.here)
			var pr, here *Project
			for _, p := range r.Resolve([]Process{inDir(10, tc.cwd), inDir(11, tc.here)}) {
				if p.ID == tc.cwd {
					pr = &p
				}
				if p.Here {
					here = &p
				}
			}
			if pr == nil || here == nil {
				t.Fatalf("project %+v, Here %+v", pr, here)
			}
			got := Project{ID: pr.ID, Root: pr.Root, Name: pr.Name, Branch: pr.Branch, Worktree: pr.Worktree, MainRepo: pr.MainRepo}
			if got != tc.want {
				t.Errorf("project %+v, want %+v", got, tc.want)
			}
			if loc := Location(pr, here); loc != tc.loc {
				t.Errorf("Location from %s = %q, want %q", tc.here, loc, tc.loc)
			}
		})
	}
}

func TestResolveLayouts(t *testing.T) {
	base := tmp(t)
	home := mkrepo(t, base, "home", "dotfiles") // dotfiles repository at $HOME
	mkdir(t, home, "notes")
	code := mkrepo(t, home, "code/app", "main")
	outside := mkrepo(t, base, "elsewhere/x", "main")

	// Submodule: .git file pointing into the superproject's .git/modules.
	super := mkrepo(t, base, "super", "main")
	mkfile(t, super, ".git/modules/sub/HEAD", "ref: refs/heads/x\n")
	mkfile(t, super, "sub/.git", "gitdir: ../.git/modules/sub\n")
	sub := filepath.Join(super, "sub")

	// Worktree with a relative gitdir (git worktree.useRelativePaths).
	mainRepo := mkrepo(t, base, "rel/main", "main")
	mkfile(t, mainRepo, ".git/worktrees/w/HEAD", "ref: refs/heads/topic\n")
	mkfile(t, mainRepo, ".git/worktrees/w/commondir", "../..\n")
	mkfile(t, base, "rel/w/.git", "gitdir: ../main/.git/worktrees/w\n")
	relWT := filepath.Join(base, "rel/w")

	// A .git file with no gitdir line is not a repository.
	bogus := mkrepo(t, base, "bogus", "main")
	mkfile(t, bogus, "in/.git", "nonsense\n")

	// Symlinked .git: refused, so the walk goes on to the enclosing repository.
	enc := mkrepo(t, base, "enc", "main")
	mkdir(t, enc, "fake")
	if err := os.Symlink(filepath.Join(code, ".git"), filepath.Join(enc, "fake/.git")); err != nil {
		t.Fatal(err)
	}
	// Symlinked parent: a process started in link/sub runs in the real directory; the kernel
	// reports the resolved path, which resolves to the real repository, not to plain/.
	mkdir(t, base, "plain")
	realRepo := mkrepo(t, base, "real", "main")
	mkdir(t, realRepo, "sub")
	if err := os.Symlink(realRepo, filepath.Join(base, "plain/link")); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(base, "plain/link/sub"))
	if err != nil {
		t.Fatal(err)
	}

	// Unreadable directory inside a repository: lstat below it fails, the walk goes on.
	locked := mkrepo(t, base, "locked", "main")
	mkdir(t, locked, "a/b")
	if err := os.Chmod(filepath.Join(locked, "a"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(locked, "a"), 0o755) })

	// Detached, hand-built.
	sha := "0123456789abcdef0123456789abcdef01234567"
	det := filepath.Join(base, "det")
	mkfile(t, det, ".git/HEAD", sha+"\n")

	gone := mkrepo(t, base, "gone", "main")

	for _, tc := range []struct {
		name, cwd string
		roots     []string
		want      Project
	}{
		{"repo at home skipped", home, nil, Project{}},
		{"below home, no repo", filepath.Join(home, "notes"), nil, Project{}},
		{"repo below home found", filepath.Join(code, "src"), nil, project(code, "main")},
		{"outside home", outside, nil, project(outside, "main")},
		{"submodule is its own project", sub, nil, Project{ID: sub, Root: sub, Name: "sub", Branch: "x", CommonDir: filepath.Join(super, ".git/modules/sub")}},
		{"relative worktree gitdir", relWT, nil, Project{ID: relWT, Root: relWT, Name: "main", Branch: "topic", Worktree: true, MainRepo: mainRepo, CommonDir: filepath.Join(mainRepo, ".git")}},
		{".git file without gitdir", filepath.Join(bogus, "in"), nil, project(bogus, "main")},
		{"symlinked .git refused", filepath.Join(enc, "fake"), nil, project(enc, "main")},
		{"symlinked parent", resolved, nil, project(realRepo, "main")},
		{"unreadable directory", filepath.Join(locked, "a/b"), nil, project(locked, "main")},
		{"detached", det, nil, Project{ID: det, Root: det, Name: "det", ShortSHA: "0123456", CommonDir: filepath.Join(det, ".git")}},
		{"cwd no longer exists", filepath.Join(gone, "removed/dir"), nil, project(gone, "main")},
		{"cwd no longer exists, no repo", filepath.Join(base, "nothing/here"), nil, Project{}},
		{"roots: inside", filepath.Join(code, "src"), []string{filepath.Join(home, "code")}, project(code, "main")},
		{"roots: root is the repo", code, []string{code}, project(code, "main")},
		{"roots: outside", outside, []string{filepath.Join(home, "code")}, Project{}},
		{"roots: sibling prefix", outside, []string{filepath.Join(base, "elsewhere/xy")}, Project{}},
		{"roots: several", outside, []string{"/nonexistent", filepath.Join(base, "elsewhere")}, project(outside, "main")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, projects := resolveOne(NewResolver(home, tc.roots), inDir(10, tc.cwd))
			var want []Project
			if tc.want.ID != "" {
				want = []Project{tc.want}
			}
			if id != tc.want.ID || !slices.Equal(projects, want) {
				t.Errorf("got %q %+v, want %+v", id, projects, want)
			}
		})
	}
}

// TestResolveRootsSymlink: --roots given through a symlink still matches the resolved cwds.
func TestResolveRootsSymlink(t *testing.T) {
	base := tmp(t)
	app := mkrepo(t, base, "code/app", "main")
	link := filepath.Join(base, "link")
	if err := os.Symlink(filepath.Join(base, "code"), link); err != nil {
		t.Fatal(err)
	}
	if id, _ := resolveOne(NewResolver("", []string{link}), inDir(10, app)); id != app {
		t.Errorf("ProjectID %q, want %q", id, app)
	}
}

func TestResolveFallbacks(t *testing.T) {
	base := tmp(t)
	shop := mkrepo(t, base, "shop", "main")
	lib := mkrepo(t, shop, "vendor/lib", "dev")
	other := mkrepo(t, base, "other", "main")
	nowhere := mkdir(t, base, "nowhere")
	noCwd := func(pid, ppid int, argv ...string) Process {
		return Process{PID: pid, PPID: ppid, Argv: argv, Unknown: FieldCwd}
	}

	procs := []Process{
		inDir(100, shop),                    // 0 resolves itself
		{PID: 101, PPID: 100, Cwd: nowhere}, // 1 parent
		{PID: 102, PPID: 101, Cwd: "/"},     // 2 grandparent
		noCwd(103, 102),                     // 3 great-grandparent
		noCwd(104, 103),                     // 4 four levels up: too far
		noCwd(105, 1, "node", "--x", "rel/path", filepath.Join(shop, "a/b.js")),             // 5 argv inside shop
		noCwd(106, 1, "vim", filepath.Join(lib, "x.go")),                                    // 6 argv inside the nested repo
		noCwd(107, 1, "cat", filepath.Join(other, "f")),                                     // 7 other is not referenced by steps 1-5
		noCwd(108, 1, "cat", "/etc/hosts", filepath.Join(shop, "x")),                        // 8 first absolute path inside a project wins
		{PID: 109, PPID: 1, Cwd: nowhere, Argv: []string{filepath.Join(shop, "../shop/y")}}, // 9 cleaned
		inDir(110, lib), // 10 nested repo resolves itself
		{PID: 0, Name: "unknown", Unknown: unknownOwner},  // 11 pseudo-process
		{PID: 111, PPID: 0, Cwd: "relative/dir"},          // 12 not absolute, parent pid 0 is not the pseudo-process
		{PID: 112, PPID: 112, Unknown: FieldCwd},          // 13 ppid loop
		{PID: 113, PPID: 1, Cwd: shop, Unknown: FieldCwd}, // 14 cwd marked unknown is ignored
	}
	procs[11].ProjectID = "stale"
	want := []string{shop, shop, shop, shop, "", shop, lib, "", shop, shop, lib, "", "", "", ""}

	projects := NewResolver("", nil).Resolve(procs)
	for i, p := range procs {
		if p.ProjectID != want[i] {
			t.Errorf("procs[%d] (pid %d): ProjectID %q, want %q", i, p.PID, p.ProjectID, want[i])
		}
	}
	if wantP := []Project{project(shop, "main"), project(lib, "dev")}; !slices.Equal(projects, wantP) {
		t.Errorf("projects %+v, want %+v", projects, wantP)
	}
}

// countLstat counts lstat calls until the test ends.
func countLstat(t *testing.T) *int {
	n := new(int)
	t.Cleanup(func() { lstat = os.Lstat })
	lstat = func(name string) (fs.FileInfo, error) {
		*n++
		return os.Lstat(name)
	}
	return n
}

func TestResolveCacheHit(t *testing.T) {
	base := tmp(t)
	shop := mkrepo(t, base, "shop", "main")
	deep := mkdir(t, shop, "a/b/c/d")
	r := NewResolver("", nil)
	n := countLstat(t)

	procs := []Process{inDir(10, deep), inDir(11, deep), inDir(12, deep)}
	r.Resolve(procs)
	miss := *n
	if miss < 6 { // .git in d, c, b, a, then shop/.git and HEAD
		t.Fatalf("first tick: %d lstat calls, want the walk", miss)
	}
	*n = 0
	r.Resolve(procs)
	if *n != 1 || procs[2].ProjectID != shop {
		t.Errorf("warm tick: %d lstat calls for one directory, want 1; ProjectID %q", *n, procs[2].ProjectID)
	}

	// A directory with no project is walked again on every tick (no stale negatives).
	*n = 0
	r.Resolve([]Process{inDir(10, base)})
	first := *n
	*n = 0
	r.Resolve([]Process{inDir(10, base)})
	if *n != first || first == 0 {
		t.Errorf("no-project directory: %d then %d lstat calls, want equal walks", first, *n)
	}
}

// TestResolveNewTick: after NewTick, every Resolve until the next NewTick shares one tick (the
// engine resolves twice per tick when argv reads are limited): the second Resolve neither walks
// a directory with no project again nor re-checks a cached project's HEAD (DEV-207). The next
// NewTick starts over: the no-project directory is walked again, so a new `git init` is seen.
func TestResolveNewTick(t *testing.T) {
	base := tmp(t)
	shop := mkrepo(t, base, "shop", "main")
	other := mkdir(t, base, "other")
	procs := []Process{inDir(10, other), inDir(11, shop)}
	r := NewResolver("", nil)
	r.Resolve(procs) // warm the cache: shop is a hit from now on
	n := countLstat(t)

	r.NewTick()
	r.Resolve(procs)
	first := *n
	if first < 2 { // HEAD of shop, then the walk up from other
		t.Fatalf("first Resolve of the tick: %d lstat calls, want a walk and a HEAD check", first)
	}
	*n = 0
	if r.Resolve(procs); *n != 0 || procs[1].ProjectID != shop || procs[0].ProjectID != "" {
		t.Errorf("second Resolve of the tick: %d lstat calls, want 0; ProjectIDs %q %q", *n, procs[0].ProjectID, procs[1].ProjectID)
	}

	mkrepo(t, base, "other", "main")
	*n = 0
	r.NewTick()
	if r.Resolve(procs); *n == 0 || procs[0].ProjectID != other {
		t.Errorf("next tick: %d lstat calls, ProjectID %q; want the walk to find %s", *n, procs[0].ProjectID, other)
	}
}

func TestResolveCacheInvalidation(t *testing.T) {
	base := tmp(t)
	shop := mkrepo(t, base, "shop", "main")
	sub := mkdir(t, shop, "sub")
	r := NewResolver("", nil)
	check := func(want Project) {
		t.Helper()
		id, projects := resolveOne(r, inDir(10, sub))
		var wantP []Project
		if want.ID != "" {
			wantP = []Project{want}
		}
		if id != want.ID || !slices.Equal(projects, wantP) {
			t.Errorf("got %q %+v, want %+v", id, projects, wantP)
		}
	}
	check(project(shop, "main"))

	// Branch switch: HEAD rewritten with a new mtime.
	head := filepath.Join(shop, ".git/HEAD")
	mkfile(t, shop, ".git/HEAD", "ref: refs/heads/feat\n")
	later := time.Now().Add(time.Second)
	if err := os.Chtimes(head, later, later); err != nil {
		t.Fatal(err)
	}
	check(project(shop, "feat"))

	// Repository removed: HEAD gone, the walk runs and finds nothing.
	if err := os.RemoveAll(filepath.Join(shop, ".git")); err != nil {
		t.Fatal(err)
	}
	check(Project{})
}

// TestResolveCacheSameMtime: HEAD replaced by rename (as git does with HEAD.lock) keeping the
// old mtime and size, as on a coarse-mtime filesystem; the new inode must still be a miss.
func TestResolveCacheSameMtime(t *testing.T) {
	base := tmp(t)
	shop := mkrepo(t, base, "shop", "main")
	head := filepath.Join(shop, ".git/HEAD")
	r := NewResolver("", nil)
	for _, branch := range []string{"main", "next", "main"} { // same length: size cannot tell them apart
		old, err := os.Lstat(head)
		if err != nil {
			t.Fatal(err)
		}
		lock := head + ".lock"
		mkfile(t, shop, ".git/HEAD.lock", "ref: refs/heads/"+branch+"\n")
		if err := os.Chtimes(lock, old.ModTime(), old.ModTime()); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(lock, head); err != nil {
			t.Fatal(err)
		}
		if _, p := resolveOne(r, inDir(10, shop)); len(p) != 1 || p[0].Branch != branch {
			t.Errorf("after HEAD → %s with the same mtime: %+v", branch, p)
		}
	}
}

// TestResolveRootsNoStat: with --roots, a cwd outside every root, or above one, costs no
// filesystem call (a hung mount outside the roots never blocks the refresh).
func TestResolveRootsNoStat(t *testing.T) {
	base := tmp(t)
	app := mkrepo(t, base, "code/app", "main")
	outside := mkrepo(t, base, "elsewhere/x", "main")
	r := NewResolver("", []string{filepath.Join(base, "code")})
	n := countLstat(t)
	for _, cwd := range []string{outside, filepath.Join(outside, "deep/dir"), base, "/"} {
		for range 2 { // every tick, not only the first
			*n = 0
			if id, _ := resolveOne(r, inDir(10, cwd)); id != "" || *n != 0 {
				t.Errorf("cwd %s: ProjectID %q, %d lstat calls, want none", cwd, id, *n)
			}
		}
	}
	if id, _ := resolveOne(r, inDir(10, filepath.Join(app, "src"))); id != app {
		t.Errorf("inside the roots: ProjectID %q, want %q", id, app)
	}
}

func TestResolveBranchSwitchGit(t *testing.T) {
	base := needGit(t)
	// Own clone of the fixture so other tests keep their branch.
	dir := tmp(t)
	if _, err := git(dir, "clone", "-q", filepath.Join(base, "shop"), "c"); err != nil {
		t.Fatal(err)
	}
	c := filepath.Join(dir, "c")
	r := NewResolver("", nil)
	if _, p := resolveOne(r, inDir(10, c)); len(p) != 1 || p[0].Branch != "main" {
		t.Fatalf("before: %+v", p)
	}
	if _, err := git(c, "switch", "-q", "-c", "next"); err != nil {
		t.Fatal(err)
	}
	if _, p := resolveOne(r, inDir(10, c)); len(p) != 1 || p[0].Branch != "next" {
		t.Errorf("after git switch: %+v, want branch next", p)
	}
	if _, err := git(c, "switch", "-q", "--detach"); err != nil {
		t.Fatal(err)
	}
	if _, p := resolveOne(r, inDir(10, c)); len(p) != 1 || p[0].Branch != "" || len(p[0].ShortSHA) != 7 {
		t.Errorf("after detach: %+v, want a short SHA", p)
	}
}

// A reftable repository keeps HEAD in its tables and writes the placeholder
// `ref: refs/heads/.invalid` to the HEAD file, for its linked worktrees too: the branch is
// unknown, not ".invalid" (DEV-152).
func TestResolveReftableGit(t *testing.T) {
	needGit(t)
	base := tmp(t)
	rt, wt := filepath.Join(base, "rt"), filepath.Join(base, "rt-wt")
	if _, err := git(base, "init", "-q", "--ref-format=reftable", "-b", "main", rt); err != nil {
		t.Skipf("git without reftable (2.45 or newer): %v", err)
	}
	for _, step := range [][]string{
		{"commit", "-q", "--allow-empty", "-m", "c"},
		{"checkout", "-q", "-b", "feat/x"},
		{"worktree", "add", "-q", "-b", "wt", wt},
	} {
		if _, err := git(rt, step...); err != nil {
			t.Fatal(err)
		}
	}
	r := NewResolver("", nil)
	for _, tc := range []struct{ cwd, label string }{{rt, "rt"}, {wt, "rt (worktree)"}} {
		_, p := resolveOne(r, inDir(10, tc.cwd))
		if len(p) != 1 || p[0].Branch != "" || p[0].ShortSHA != "" || p[0].Label() != tc.label {
			t.Errorf("%s: %+v, want label %q with no branch and no SHA", tc.cwd, p, tc.label)
		}
	}
}

func TestResolveCacheCap(t *testing.T) {
	base := tmp(t)
	shop := mkrepo(t, base, "shop", "main")
	r := NewResolver("", nil)
	procs := make([]Process, maxCache+500)
	for i := range procs {
		// Directories need not exist: the walk climbs to shop.
		procs[i] = inDir(i+1, filepath.Join(shop, fmt.Sprint("d", i)))
	}
	projects := r.Resolve(procs)
	if len(r.cache) != maxCache {
		t.Errorf("cache holds %d entries, want %d", len(r.cache), maxCache)
	}
	if len(projects) != 1 || procs[len(procs)-1].ProjectID != shop {
		t.Errorf("projects %+v, last ProjectID %q", projects, procs[len(procs)-1].ProjectID)
	}
	// Evicted directories still resolve.
	if projects = r.Resolve(procs); len(projects) != 1 || len(r.cache) != maxCache {
		t.Errorf("second tick: projects %+v, cache %d", projects, len(r.cache))
	}
}

// hereIDs returns the IDs of the projects marked Here.
func hereIDs(projects []Project) []string {
	var ids []string
	for _, p := range projects {
		if p.Here {
			ids = append(ids, p.ID)
		}
	}
	return ids
}

// TestResolveHereGit: devdash's own directory marks its project, and only that one: in a
// linked worktree, the worktree's project, not the main repository's.
func TestResolveHereGit(t *testing.T) {
	base := needGit(t)
	shop := filepath.Join(base, "shop")
	wt := filepath.Join(base, "shop-wt")
	lib := filepath.Join(shop, "vendor/lib")
	for _, tc := range []struct {
		name, here string
		want       []string
	}{
		{"main repo root", shop, []string{shop}},
		{"main repo subdirectory", filepath.Join(shop, "sub/deep"), []string{shop}},
		{"nested repo", lib, []string{lib}},
		{"linked worktree", wt, []string{wt}},
		{"outside any repo", base, nil},
		{"no working directory", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewResolver("", nil)
			r.SetHere(tc.here)
			procs := []Process{inDir(10, shop), inDir(11, wt), inDir(12, lib), inDir(13, base)}
			for tick := range 2 { // every snapshot, not only the first
				projects := r.Resolve(procs)
				if len(projects) != 3 {
					t.Fatalf("tick %d: projects %+v, want shop, its worktree and lib", tick, projects)
				}
				if got := hereIDs(projects); !slices.Equal(got, tc.want) {
					t.Errorf("tick %d: Here on %q, want %q", tick, got, tc.want)
				}
			}
		})
	}
}

// TestResolveHereLayouts: the Here directory follows the rules for a process cwd (steps 2-4):
// the $HOME rule, --roots, symlinks resolved.
func TestResolveHereLayouts(t *testing.T) {
	base := tmp(t)
	home := mkrepo(t, base, "home", "dotfiles") // dotfiles repository at $HOME
	notes := mkdir(t, home, "notes")
	code := mkrepo(t, home, "code/app", "main")
	src := mkdir(t, code, "src")
	outside := mkrepo(t, base, "elsewhere/x", "main")
	link := filepath.Join(base, "link")
	if err := os.Symlink(src, link); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, here string
		roots      []string
		want       []string
	}{
		{"repo below home", src, nil, []string{code}},
		{"dotfiles repo at home is not a project", home, nil, nil},
		{"below home, no repo", notes, nil, nil},
		{"outside home", outside, nil, []string{outside}},
		{"symlinked directory", link, nil, []string{code}},
		{"directory removed", filepath.Join(outside, "gone/dir"), nil, []string{outside}},
		{"relative path", "elsewhere/x", nil, nil},
		{"roots: inside", code, []string{filepath.Join(home, "code")}, []string{code}},
		{"roots: here excluded", outside, []string{filepath.Join(home, "code")}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewResolver(home, tc.roots)
			r.SetHere(tc.here)
			// A process in each candidate, so that every project the Here directory could
			// name is referenced (the dotfiles repository is never a project).
			projects := r.Resolve([]Process{inDir(10, code), inDir(11, outside), inDir(12, home)})
			if got := hereIDs(projects); !slices.Equal(got, tc.want) {
				t.Errorf("Here on %q, want %q (projects %+v)", got, tc.want, projects)
			}
		})
	}
}

// TestResolveHereBranchSwitch: the Here project carries the branch of the current snapshot
// and stays Here across a switch.
func TestResolveHereBranchSwitch(t *testing.T) {
	base := tmp(t)
	shop := mkrepo(t, base, "shop", "main")
	r := NewResolver("", nil)
	r.SetHere(shop)
	if _, p := resolveOne(r, inDir(10, shop)); len(p) != 1 || !p[0].Here || p[0].Branch != "main" {
		t.Fatalf("before: %+v", p)
	}
	mkfile(t, shop, ".git/HEAD", "ref: refs/heads/feat\n")
	later := time.Now().Add(time.Second)
	if err := os.Chtimes(filepath.Join(shop, ".git/HEAD"), later, later); err != nil {
		t.Fatal(err)
	}
	if _, p := resolveOne(r, inDir(10, shop)); len(p) != 1 || !p[0].Here || p[0].Branch != "feat" {
		t.Errorf("after the switch: %+v, want branch feat and Here", p)
	}
}

// TestResolveHereCached: with a process in the Here directory, as devdash itself always is,
// marking Here costs no filesystem call: a warm tick is still one lstat.
func TestResolveHereCached(t *testing.T) {
	base := tmp(t)
	shop := mkrepo(t, base, "shop", "main")
	sub := mkdir(t, shop, "a/b")
	r := NewResolver("", nil)
	r.SetHere(sub)
	resolveOne(r, inDir(10, sub))
	n := countLstat(t)
	if _, p := resolveOne(r, inDir(10, sub)); *n != 1 || len(p) != 1 || !p[0].Here {
		t.Errorf("warm tick: %d lstat calls, want 1; projects %+v", *n, p)
	}
}

// TestBuildHere: in a snapshot, the Here project referenced by devdash's own process is
// marked and the other project is not, in every snapshot; a resolver without a Here
// directory marks nothing.
func TestBuildHere(t *testing.T) {
	base := tmp(t)
	shop := mkrepo(t, base, "shop", "main")
	other := mkrepo(t, base, "other", "main")
	r := NewResolver("", nil)
	r.SetHere(filepath.Join(shop, "cmd"))
	raw := Raw{TakenAt: t0, Processes: []Process{
		{PID: 10, PPID: 1, Name: "vite", Argv: []string{"vite"}, Cwd: other},
		{PID: 11, PPID: 1, Name: "devdash", Argv: []string{"devdash"}, Cwd: shop},
	}}
	here := project(shop, "main")
	here.Here = true
	want := []Project{project(other, "main"), here}

	s := Build(raw, Snapshot{}, nil, r)
	if !slices.Equal(s.Projects, want) || s.Processes[1].ProjectID != shop {
		t.Errorf("projects %+v, devdash in %q; want %+v, devdash in %q", s.Projects, s.Processes[1].ProjectID, want, shop)
	}
	if next := Build(raw, s, nil, r); !slices.Equal(next.Projects, want) {
		t.Errorf("next snapshot: projects %+v, want %+v", next.Projects, want)
	}
	if got := hereIDs(Build(raw, s, nil, NewResolver("", nil)).Projects); got != nil {
		t.Errorf("no Here directory: Here on %q", got)
	}
}

// BenchmarkResolve: 500 processes over 5 repositories with 20 directories each, warm cache.
func BenchmarkResolve(b *testing.B) {
	base, err := filepath.EvalSymlinks(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	var procs []Process
	for i := range 500 {
		repo := filepath.Join(base, fmt.Sprint("repo", i%5))
		dir := filepath.Join(repo, "src", fmt.Sprint("pkg", i%20))
		if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, ".git/HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
			b.Fatal(err)
		}
		if i%10 == 0 {
			dir = "/" // system processes: no project
		}
		procs = append(procs, Process{PID: i + 2, PPID: 1, Cwd: dir})
	}
	r := NewResolver("", nil)
	r.Resolve(procs)
	for b.Loop() {
		r.Resolve(procs)
	}
}
