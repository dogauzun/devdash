package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/dogauzun/devdash/internal/model"
)

// detailSelect selects the row with key k as if the user had moved to it.
func detailSelect(t *testing.T, m *Model, k model.RowKey) {
	t.Helper()
	m.sel = k
	m.rebuild()
	if r, ok := m.selected(); !ok || r.Key != k {
		t.Fatalf("row %+v is not in the table", k)
	}
}

// bodyLines returns the screen lines between the header and the footer.
func bodyLines(m *Model) []string {
	lines := strings.Split(screen(m), "\n")
	return lines[1 : len(lines)-strings.Count(m.footerView(m.width), "\n")-1]
}

// hasLine fails the test unless the screen has a line equal to want.
func hasLine(t *testing.T, m *Model, want string) {
	t.Helper()
	for l := range strings.SplitSeq(screen(m), "\n") {
		if l == want {
			return
		}
	}
	t.Errorf("no line %q on screen:\n%s", want, screen(m))
}

func TestDetailWidth(t *testing.T) {
	for w, want := range map[int]int{120: 40, 150: 60, 200: 80, 300: 80} {
		got := detailWidth(w)
		if got != want {
			t.Errorf("detailWidth(%d) = %d, want %d", w, got, want)
		}
		if w-got < minWidth {
			t.Errorf("at %d the table keeps %d columns, want at least %d", w, w-got, minWidth)
		}
	}
}

func TestDetailSplit(t *testing.T) {
	m, _ := newTest(t, 120, 30)
	s := fixture()
	feed(m, s)
	detailSelect(t, m, keyOf(s, 200))
	press(m, "enter")
	body := bodyLines(m)
	// Every body line: the table in the first 80 columns, then the pane behind a plain bar.
	for i, l := range body {
		if right := ansi.Cut(l, 80, 120); !strings.HasPrefix(right, "|") {
			t.Errorf("body line %d: pane column %q does not start with the separator", i, right)
		}
	}
	if left := strings.TrimSpace(ansi.Cut(body[0], 0, 80)); left == "" {
		t.Errorf("first body line has no table on the left: %q", body[0])
	}
	l := line(m, "api 200 · server")
	if got := ansi.Cut(l, 80, 120); got != "| api 200 · server" {
		t.Errorf("pane title %q, want it at column 80 behind the separator", got)
	}
	if line(m, "shop") == "" {
		t.Error("the table is not shown next to the pane")
	}

	// In the 38-column value area a note wraps as a whole, onto an indented line.
	detailSelect(t, m, keyOf(s, 101))
	for _, want := range []string{"| listeners tcp6 [::]:5173", "|           (every interface)"} {
		if got := ansi.Cut(line(m, want), 80, 120); got != want {
			t.Errorf("pane line %q, want %q", got, want)
		}
	}
}

func TestDetailOverlay(t *testing.T) {
	for _, w := range []int{80, 119} {
		m, _ := newTest(t, w, 30)
		s := fixture()
		feed(m, s)
		detailSelect(t, m, keyOf(s, 200))
		press(m, "enter")
		if got := bodyLines(m)[0]; !strings.HasPrefix(got, "api 200 · server") {
			t.Errorf("width %d: overlay starts with %q, want the pane title at column 0", w, got)
		}
		if strings.Contains(screen(m), "shop") || strings.Contains(screen(m), "|") {
			t.Errorf("width %d: the table or the split separator is still drawn:\n%s", w, screen(m))
		}
		// The selection keys still move; the pane follows the selection.
		press(m, "down")
		if got := bodyLines(m)[0]; !strings.HasPrefix(got, "go 201 · test") {
			t.Errorf("width %d: after down the overlay shows %q, want go 201", w, got)
		}
		for _, k := range []string{"esc", "enter"} {
			press(m, k)
			if line(m, "shop") == "" {
				t.Errorf("width %d: %s does not close the overlay", w, k)
			}
			press(m, "enter")
		}
	}
}

func TestDetailProcess(t *testing.T) {
	m, _ := newTest(t, 100, 40)
	s := fixture()
	feed(m, s)
	detailSelect(t, m, keyOf(s, 101))
	press(m, "enter")
	for _, want := range []string{
		"node 101 · server",
		"command   node node_modules/.bin/vite --port 5173",
		"cwd       /src/shop",
		"project   shop @ feat/cart (worktree)",
		"root      /src/shop",
		"main repo /src/shop-main",
		"listeners tcp6 [::]:5173 (every interface)",
		"parents   zsh 100 < pid 90",
		"started   " + at(3*time.Hour).Local().Format(time.DateTime) + " (3 h ago)",
		"user      me (uid 501)",
		"cpu       12.3%",
		"mem       178.9 MiB",
	} {
		hasLine(t, m, want)
	}
	if strings.Contains(screen(m), "unknown") || strings.Contains(screen(m), "docker socket") {
		t.Errorf("a fully read host process shows unknown fields or a socket:\n%s", screen(m))
	}

	// Two listeners, one line each; a first CPU sample; a process outside every project.
	detailSelect(t, m, keyOf(s, 200))
	hasLine(t, m, "listeners tcp4 127.0.0.1:8080")
	hasLine(t, m, "          tcp6 [::1]:8081")
	hasLine(t, m, "parents   sshd 1")
	detailSelect(t, m, keyOf(s, 103))
	hasLine(t, m, "cpu       –")
	hasLine(t, m, "listeners none")
	detailSelect(t, m, keyOf(s, 1))
	hasLine(t, m, "project   none")
	hasLine(t, m, "cwd       unknown")
	hasLine(t, m, "user      root (uid 0)")
	hasLine(t, m, "parents   none")
	hasLine(t, m, "unknown   cwd")
}

func TestDetailParentChain(t *testing.T) {
	p := func(pid, ppid int, start time.Duration, name string) model.Process {
		return model.Process{PID: pid, PPID: ppid, StartTime: at(start), Name: name, Argv: []string{name}}
	}
	for _, tc := range []struct {
		name  string
		procs []model.Process
		want  string
	}{
		{"up to the root", []model.Process{p(1, 0, 99*time.Hour, "launchd"), p(90, 1, 10*time.Hour, "tmux"),
			p(100, 90, 5*time.Hour, "zsh"), p(101, 100, time.Hour, "node")}, "zsh 100 < tmux 90 < launchd 1"},
		{"parent not in the snapshot", []model.Process{p(100, 90, 5*time.Hour, "zsh"), p(101, 100, time.Hour, "node")},
			"zsh 100 < pid 90"},
		{"reused parent pid", []model.Process{p(100, 90, time.Minute, "zsh"), p(101, 100, time.Hour, "node")},
			"pid 100"},
		{"cycle", []model.Process{p(100, 101, time.Hour, "a"), p(101, 100, time.Hour, "node")}, "a 100 < pid 101"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTest(t, 100, 30)
			s := model.Snapshot{SchemaVersion: 1, TakenAt: at(0), Processes: tc.procs}
			feed(m, s)
			detailSelect(t, m, keyOf(s, 101))
			press(m, "enter")
			hasLine(t, m, "parents   "+tc.want)
		})
	}
}

func TestDetailContainer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		socket func() string
		want   string // "" for no socket line
	}{
		{"socket known", func() string { return "/var/run/docker.sock" }, "docker socket: /var/run/docker.sock"},
		{"no socket func", nil, ""},
		{"empty socket", func() string { return "" }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTest(t, 100, 40, func(o *Options) { o.DockerSocket = tc.socket })
			s := fixture()
			feed(m, s)
			press(m, "enter")

			// docker-proxy holding shop-db-1's published port.
			detailSelect(t, m, keyOf(s, 300))
			for _, want := range []string{
				"docker-proxy 300 · container",
				"cwd       unknown",
				"unknown   cwd, cpu, mem",
				"container shop-db-1",
				"image     postgres:16",
				"state     running",
				"compose   shop / db",
				"ports     0.0.0.0:5432 -> 5432/tcp",
			} {
				hasLine(t, m, want)
			}
			if tc.want != "" {
				hasLine(t, m, tc.want)
			} else if strings.Contains(screen(m), "docker socket") {
				t.Errorf("socket line shown without a socket:\n%s", screen(m))
			}

			// A container with no process behind it.
			detailSelect(t, m, model.RowKey{ContainerID: "4e5d6c7b8a90"})
			for _, want := range []string{
				"shop-web-1 · container",
				"container shop-web-1",
				"image     nginx:1.27",
				"ports     0.0.0.0:8000 -> 80/tcp",
				"no process holds the port (published by Docker)",
			} {
				hasLine(t, m, want)
			}
			if tc.want != "" {
				hasLine(t, m, tc.want)
			}

			// A host process is not a container row: no socket line even when one is known.
			detailSelect(t, m, keyOf(s, 200))
			if strings.Contains(screen(m), "docker socket") {
				t.Errorf("socket shown for a host process:\n%s", screen(m))
			}
		})
	}
}

func TestDetailWrap(t *testing.T) {
	m, _ := newTest(t, 50, 30)
	s := fixture()
	feed(m, s)
	detailSelect(t, m, keyOf(s, 102))
	press(m, "enter")
	// The value column is 40 wide: the path is cut at 40 cells, the flags fill the next line.
	hasLine(t, m, "command   /src/shop/node_modules/@esbuild/darwin-a")
	hasLine(t, m, "          rm64/bin/esbuild --service=0.21.5 --ping")

	// Word wrapping keeps whole arguments together.
	s.Processes[1].Argv = []string{"node", "node_modules/.bin/vite", "--port", "5173", "--host", "0.0.0.0", "--strictPort"}
	feed(m, s)
	detailSelect(t, m, keyOf(s, 101))
	hasLine(t, m, "command   node node_modules/.bin/vite --port 5173")
	hasLine(t, m, "          --host 0.0.0.0 --strictPort")

	// Unknown argv.
	s.Processes[1].Argv, s.Processes[1].Unknown = nil, model.FieldArgv
	feed(m, s)
	hasLine(t, m, "command   unknown")
}

func TestDetailWords(t *testing.T) {
	for _, tc := range []struct {
		words []string
		sep   string
		width int
		want  []string
	}{
		{[]string{"a", "bb", "ccc"}, " ", 10, []string{"a bb ccc"}},
		{[]string{"a", "bb", "ccc"}, " ", 4, []string{"a bb", "ccc"}},
		{[]string{"abcdefghij"}, " ", 4, []string{"abcd", "efgh", "ij"}},
		{[]string{"x", "abcdefg"}, " ", 4, []string{"x", "abcd", "efg"}},
		{[]string{"node 101", "zsh 100"}, " < ", 12, []string{"node 101 <", "zsh 100"}},
		{nil, " ", 4, []string{""}},
		{[]string{"abc"}, " ", 0, []string{"abc"}},
	} {
		if got := detailWrap(tc.words, tc.sep, tc.width); strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("detailWrap(%q, %q, %d) = %q, want %q", tc.words, tc.sep, tc.width, got, tc.want)
		}
	}
}

func TestDetailArgvQuoting(t *testing.T) {
	got := detailArgv([]string{"sh", "-c", "echo hi", "", "\x1b[31mred"})
	want := []string{"sh", "-c", `"echo hi"`, `""`, `"\x1b[31mred"`}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("detailArgv = %q, want %q", got, want)
	}
}

func TestDetailHeader(t *testing.T) {
	m, _ := newTest(t, 100, 30)
	s := fixture()
	s.Projects[1].Branch, s.Projects[1].ShortSHA = "", "abc1234"
	feed(m, s)
	press(m, "enter")

	detailSelect(t, m, model.RowKey{Header: model.GroupProject, Group: shopID})
	for _, want := range []string{"shop @ feat/cart (worktree)", "root      /src/shop", "branch    feat/cart",
		"main repo /src/shop-main"} {
		hasLine(t, m, want)
	}
	detailSelect(t, m, model.RowKey{Header: model.GroupProject, Group: apiID})
	hasLine(t, m, "api @ abc1234")
	hasLine(t, m, "branch    detached at abc1234")
	if strings.Contains(screen(m), "main repo") {
		t.Error("main repo shown for a project that is not a worktree")
	}
	detailSelect(t, m, model.RowKey{Header: model.GroupCompose, Group: "shop"})
	hasLine(t, m, "shop (compose)")
	hasLine(t, m, "members   shop-db-1, shop-web-1")
	detailSelect(t, m, model.RowKey{Header: model.GroupOther})
	hasLine(t, m, "other")
	hasLine(t, m, "processes outside every project")
}

func TestDetailUnknownOwner(t *testing.T) {
	m, _ := newTest(t, 100, 30)
	s := fixture()
	feed(m, s)
	detailSelect(t, m, keyOf(s, 0))
	press(m, "enter")
	for _, want := range []string{
		"unknown owner",
		"the process holding this port could not be read",
		"listeners tcp4 0.0.0.0:631 (every interface)",
		"hint      run with sudo to see owners",
	} {
		hasLine(t, m, want)
	}
}

func TestDetailNothingSelected(t *testing.T) {
	for _, w := range []int{80, 120} {
		m, _ := newTest(t, w, 24)
		press(m, "enter")
		if line(m, "nothing selected") == "" {
			t.Errorf("width %d: empty table, open pane:\n%s", w, screen(m))
		}
	}
}
