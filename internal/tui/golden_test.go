package tui

import (
	"context"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/dogauzun/devdash/internal/collector"
	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
)

// Golden views (DEV-35): View() at 80x24 and 120x40, compared with testdata/<name>.golden.
// Rewrite them with: go test ./internal/tui -run TestGolden -update
//
// Every view is stored as plain text (styles stripped, trailing spaces trimmed, as screen
// returns it). Some are also stored styled: View().Content passed through colorprofile's
// writer with a fixed profile, ANSI for colour on (<name>.ansi.golden) and Ascii for colour
// off (<name>.ascii.golden), the downsampling the program applies on output (tea.WithColorProfile).
// Lip Gloss renders styles without looking at the terminal, so the output is the same on
// every machine; the clock is fixed and the hostname comes from the snapshot.

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

// golden compares got with testdata/name, or rewrites it with -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	got += "\n"
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("view differs from %s (go test ./internal/tui -run TestGolden -update):\n%s", path, got)
	}
}

// styled is the view's content downsampled to profile p, as the program writes it.
func styled(t *testing.T, m *Model, p colorprofile.Profile) string {
	t.Helper()
	var b strings.Builder
	w := &colorprofile.Writer{Forward: &b, Profile: p}
	if _, err := w.WriteString(m.View().Content); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// goldenUsers names the engine scenario's uids without asking the OS.
var goldenUsers = map[int]string{0: "root", 501: "me"}

// goldenDocker is the engine scenario's Docker: the fixture's two containers.
type goldenDocker struct{}

func (goldenDocker) Fetch(context.Context) ([]model.Container, *model.Warning) {
	return fixture().Containers, nil
}

// engineModel returns a model at w by h showing what a real engine built from collector.Fake:
// the fixture's machine (a worktree shop on feat/cart and api on main in temporary
// repositories, a compose project, sshd and an unknown owner) sampled twice, 2 s apart, so CPU
// is a number except for claude, which appears in the second sample. The model takes the
// engine's updates through Source as runTUI wires it, inside a synctest bubble so the second
// tick comes without waiting; it shows the second one, by which time Docker's first Fetch has
// returned. Argv holds no temporary path, so the views do not depend on where the
// repositories are.
func engineModel(t *testing.T, w, h int) *Model {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir()) // the kernel reports resolved cwds
	if err != nil {
		t.Fatal(err)
	}
	write := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	shopMain, shop, api := filepath.Join(dir, "shop"), filepath.Join(dir, "shop-cart"), filepath.Join(dir, "api")
	write(filepath.Join(shopMain, ".git", "HEAD"), "ref: refs/heads/main\n")
	write(filepath.Join(shopMain, ".git", "worktrees", "cart", "HEAD"), "ref: refs/heads/feat/cart\n")
	write(filepath.Join(shop, ".git"), "gitdir: "+filepath.Join(shopMain, ".git", "worktrees", "cart")+"\n")
	write(filepath.Join(shop, "web", "index.html"), "")
	write(filepath.Join(api, ".git", "HEAD"), "ref: refs/heads/main\n")

	// sample is the collector's answer at at; cpu is the CPU time every process used up to it.
	sample := func(at time.Time, cpu time.Duration, second bool) collector.Step {
		proc := func(pid, ppid int, start time.Time, uid int, cwd string, argv ...string) collector.Process {
			return collector.Process{PID: pid, PPID: ppid, StartTime: start, UID: uid, Name: filepath.Base(argv[0]),
				Argv: argv, Cwd: cwd, CPUTime: cpu, RSSBytes: 50 << 20}
		}
		vite := proc(101, 100, now.Add(-3*time.Hour), 501, shop, "node", "node_modules/.bin/vite", "--port", "5173")
		vite.CPUTime, vite.RSSBytes = 24*cpu+6*cpu/10, 187563008 // 12.3% against the others' 0.5%
		proxy := proc(300, 1, now.Add(-4*time.Hour), 0, "", "/usr/bin/docker-proxy", "-proto", "tcp", "-host-ip", "0.0.0.0", "-host-port", "5432")
		proxy.Unknown = model.FieldCwd | model.FieldCPU | model.FieldMem
		sshd := proc(1, 0, now.Add(-72*time.Hour), 0, "", "/usr/sbin/sshd", "-D")
		sshd.Unknown = model.FieldCwd
		procs := []collector.Process{
			proc(100, 90, now.Add(-5*time.Hour), 501, shop, "-zsh"),
			vite,
			proc(102, 101, now.Add(-3*time.Hour+time.Second), 501, shop,
				"node_modules/@esbuild/darwin-arm64/bin/esbuild", "--service=0.21.5", "--ping"),
			proc(200, 1, now.Add(-26*time.Hour), 501, api, "bin/api", "-addr", ":8080"),
			proc(201, 1, now.Add(-30*time.Second), 501, api, "go", "test", "./..."),
			proc(202, 1, now.Add(-2*time.Hour), 501, api, "nvim", "main.go"),
			proxy,
			sshd,
		}
		procs[0].Name = "zsh"
		if second {
			procs = append(procs, proc(103, 90, now.Add(-20*time.Minute), 501, filepath.Join(shop, "web"), "claude"))
		}
		return collector.Step{Result: collector.Result{
			TakenAt:   at,
			Host:      model.Host{OS: "darwin", Arch: "arm64", Hostname: "mbp", UID: 501},
			Processes: procs,
			Listeners: []collector.Listener{
				{Proto: "tcp6", Addr: netip.IPv6Unspecified(), Port: 5173, PID: 101},
				{Proto: "tcp4", Addr: netip.MustParseAddr("127.0.0.1"), Port: 8080, PID: 200},
				{Proto: "tcp6", Addr: netip.IPv6Loopback(), Port: 8081, PID: 200},
				{Proto: "tcp4", Addr: netip.IPv4Unspecified(), Port: 5432, PID: 300},
				{Proto: "tcp4", Addr: netip.IPv4Unspecified(), Port: 22, PID: 1},
				{Proto: "tcp4", Addr: netip.IPv4Unspecified(), Port: 631, PID: 0},
			},
			Warnings: []model.Warning{{Code: "listener_owner_unreadable", Count: 1, Hint: "run with sudo to see owners"}},
		}}
	}

	var m *Model
	synctest.Test(t, func(t *testing.T) {
		e := engine.New(engine.Options{
			Collector: &collector.Fake{Steps: []collector.Step{
				sample(now.Add(-4*time.Second), time.Second, false),
				sample(now.Add(-2*time.Second), time.Second+10*time.Millisecond, true),
			}},
			Resolver: model.NewResolver("", nil),
			LookupUser: func(uid int) string {
				n, ok := goldenUsers[uid]
				if !ok {
					t.Errorf("uid %d not in the golden user table", uid)
				}
				return n
			},
			Docker: goldenDocker{},
		})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); e.Run(ctx) }()

		m = New(goldenOptions(t, e))
		m.Update(tea.WindowSizeMsg{Width: w, Height: h})
		for range 2 {
			m.Update(m.wait()())
		}
		cancel()
		<-done
	})
	if s := m.upd.Snapshot; !s.TakenAt.Equal(now.Add(-2*time.Second)) || len(s.Containers) != 2 {
		t.Fatalf("engine snapshot taken %v with %d containers, want the second sample with Docker's two", s.TakenAt, len(s.Containers))
	}
	return m
}

// goldenOptions are the options of every golden model: a fixed clock, and plan, kill and open
// that fail the test, since showing a view never plans, signals or opens anything.
func goldenOptions(t *testing.T, src Source) Options {
	return Options{
		Source: src,
		Now:    func() time.Time { return now },
		Plan: func(model.Snapshot, model.RowKey, engine.KillOptions) (engine.Plan, error) {
			t.Error("unexpected Plan call")
			return engine.Plan{}, nil
		},
		Kill: failKill(t),
		Open: func(string) error { t.Error("unexpected Open call"); return nil },
	}
}

// failKill is a Kill that fails the test: New defaults a nil Kill to the real engine.Kill, so
// a test that does not mean to kill passes this rather than nothing.
func failKill(t *testing.T) func(engine.Plan, time.Duration) (engine.Result, error) {
	return func(p engine.Plan, _ time.Duration) (engine.Result, error) {
		t.Errorf("unexpected Kill call with %+v: tests never signal", p)
		return engine.Result{}, nil
	}
}

// TestGoldenTable: the table as the engine's snapshot fills it, coloured and not.
func TestGoldenTable(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		w, h := size[0], size[1]
		name := "table-" + sizeName(w, h)
		t.Run(name, func(t *testing.T) {
			m := engineModel(t, w, h)
			golden(t, name+".golden", screen(m))
			golden(t, name+".ansi.golden", styled(t, m, colorprofile.ANSI))
			golden(t, name+".ascii.golden", styled(t, m, colorprofile.Ascii))
		})
	}
}

func sizeName(w, h int) string { return strconv.Itoa(w) + "x" + strconv.Itoa(h) }

// TestGoldenViews: the detail pane, the filter and the kill modal over fixture(), whose
// processes, projects and containers the engine scenario above mirrors. Plans come from
// fakePlanner and kills go to fakeKiller, which must stay unused: no view confirms.
func TestGoldenViews(t *testing.T) {
	type view struct {
		name        string
		w, h        int
		postgres    bool // add withPostgres's server outside every project
		do          func(t *testing.T, m *Model, s model.Snapshot)
		ansi, ascii bool // also store these styled forms
	}
	selectPID := func(pid int, keys ...string) func(*testing.T, *Model, model.Snapshot) {
		return func(t *testing.T, m *Model, s model.Snapshot) {
			selectRow(t, m, keyOf(s, pid))
			press(m, keys...)
		}
	}
	filter := func(q string, keys ...string) func(*testing.T, *Model, model.Snapshot) {
		return func(t *testing.T, m *Model, _ model.Snapshot) {
			press(m, "/")
			typeText(m, q)
			press(m, keys...)
		}
	}
	for _, v := range []view{
		{name: "detail-80x24", w: 80, h: 24, do: selectPID(101, "enter")},               // full-screen overlay
		{name: "detail-120x40", w: 120, h: 40, do: selectPID(101, "enter"), ansi: true}, // right split
		{name: "filter-80x24", w: 80, h: 24, do: filter("vite", "enter")},               // applied
		{name: "filter-120x40", w: 120, h: 40, do: filter("sh")},                        // prompt still open
		{name: "kill-80x24", w: 80, h: 24, do: selectPID(101, "x", "t"), ansi: true, ascii: true},
		{name: "kill-120x40", w: 120, h: 40, do: selectPID(101, "x", "t")},
		// The second confirmation; postgres rather than sshd, since the engine refuses pid 1.
		{name: "kill-outside-80x24", w: 80, h: 24, postgres: true, do: selectPID(400, "x", "enter"), ansi: true},
	} {
		t.Run(v.name, func(t *testing.T) {
			s := fixture()
			if v.postgres {
				s, _ = withPostgres(s)
			}
			m, _, _, fk := newKillTest(t, v.w, v.h, s)
			v.do(t, m, s)
			golden(t, v.name+".golden", screen(m))
			if v.ansi {
				golden(t, v.name+".ansi.golden", styled(t, m, colorprofile.ANSI))
			}
			if v.ascii {
				golden(t, v.name+".ascii.golden", styled(t, m, colorprofile.Ascii))
			}
			if len(fk.plans) != 0 {
				t.Errorf("Kill called with %+v", fk.plans)
			}
		})
	}
}
