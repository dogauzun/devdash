package tui

import (
	"fmt"
	"math"
	"slices"
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

// TestDetailStartedLocalZone: the start time is shown in the machine's zone, and the tests'
// pinned zone (TestMain) is what keeps the detail goldens the same on every machine (DEV-90).
func TestDetailStartedLocalZone(t *testing.T) {
	if time.Local != time.UTC {
		t.Fatalf("time.Local is %v, want UTC pinned by TestMain so the goldens do not depend on TZ", time.Local)
	}
	for _, c := range []struct {
		zone *time.Location
		want string
	}{
		{time.UTC, "started   2026-10-02 09:00:00 (3 h ago)"},
		{time.FixedZone("UTC+3", 3*60*60), "started   2026-10-02 12:00:00 (3 h ago)"},
		{time.FixedZone("UTC-7", -7*60*60), "started   2026-10-02 02:00:00 (3 h ago)"},
	} {
		t.Run(c.zone.String(), func(t *testing.T) {
			pinLocal(t, c.zone)
			m, _ := newTest(t, 100, 40)
			s := fixture()
			feed(m, s)
			detailSelect(t, m, keyOf(s, 101))
			press(m, "enter")
			hasLine(t, m, c.want)
		})
	}
}

func TestDetailProcess(t *testing.T) {
	m, _ := newTest(t, 100, 40)
	s := fixture()
	feed(m, s)
	detailSelect(t, m, keyOf(s, 101))
	press(m, "enter")
	for _, want := range []string{
		"vite (node) 101 · server",
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

func TestDetailWrapWideAtWidthOne(t *testing.T) {
	// A grapheme wider than the width gets a line of its own instead of an empty cut. Each call
	// runs in a goroutine: the bug this guards against was an endless loop.
	for _, tc := range []struct {
		words []string
		want  []string
	}{
		{[]string{"ab漢字c"}, []string{"a", "b", "漢", "字", "c"}},
		{[]string{"漢字", "x"}, []string{"漢", "字", "x"}},
	} {
		done := make(chan []string, 1)
		go func() { done <- detailWrap(tc.words, " ", 1) }()
		select {
		case got := <-done:
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("detailWrap(%q, \" \", 1) = %q, want %q", tc.words, got, tc.want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("detailWrap(%q, \" \", 1) does not return", tc.words)
		}
	}
	// The 11-column overlay leaves a 1-cell value column; the too-wide runes are cut by the
	// overlay, but the pane renders.
	m, _ := newTest(t, 11, 24)
	s := fixture()
	s.Processes[4].Argv = []string{"漢字"}
	feed(m, s)
	detailSelect(t, m, keyOf(s, 200))
	press(m, "enter")
	if line(m, "command") == "" {
		t.Errorf("the 11-column overlay does not show the command field:\n%s", screen(m))
	}
}

// hiddenProxy is the fixture as a non-root user in the docker group sees it: docker-proxy is
// root's, so shop-db-1's published port 5432 has a PID 0 owner that Reconcile gave the
// container's ID (DEV-89).
func hiddenProxy() model.Snapshot {
	s := fixture()
	s.Processes = slices.DeleteFunc(s.Processes, func(p model.Process) bool { return p.PID == 300 })
	l := lis("tcp4", "0.0.0.0", 5432)
	l.ContainerID = "9f1c2a7b0d3e"
	s.Processes = append(s.Processes, model.Process{Name: "unknown", Kind: model.KindContainer,
		ContainerID: "9f1c2a7b0d3e", Listeners: []model.Listener{l}, CPUPercent: math.NaN(),
		Unknown: model.FieldOwner | model.FieldArgv | model.FieldCwd | model.FieldCPU | model.FieldMem})
	return s
}

func TestDetailUnknownOwnerContainer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		socket func() string
		want   string // "" for no socket line
	}{
		{"socket known", func() string { return "/var/run/docker.sock" }, "docker socket: /var/run/docker.sock"},
		{"no socket", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTest(t, 100, 40, func(o *Options) { o.DockerSocket = tc.socket })
			s := hiddenProxy()
			feed(m, s)
			press(m, "enter")
			detailSelect(t, m, s.Processes[len(s.Processes)-1].Key())
			for _, want := range []string{
				"shop-db-1 · container",
				"listeners tcp4 0.0.0.0:5432 (every interface)",
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
			}
			for _, not := range []string{"unknown owner", "sudo", "no process holds the port"} {
				if strings.Contains(strings.Join(bodyLines(m), "\n"), not) {
					t.Errorf("container's port shows %q:\n%s", not, screen(m))
				}
			}

			// Docker's list no longer has the container: its ID stands in for the name.
			s.Containers = s.Containers[1:]
			feed(m, s)
			detailSelect(t, m, s.Processes[len(s.Processes)-1].Key())
			hasLine(t, m, "container 9f1c2a7b0d3e")
			if strings.Contains(strings.Join(bodyLines(m), "\n"), "sudo") {
				t.Errorf("container's port shows the sudo hint:\n%s", screen(m))
			}
		})
	}
}

func TestDetailSplitNotesWrap(t *testing.T) {
	// At 120 columns the pane's content is 38 wide: free-text notes wrap at spaces.
	m, _ := newTest(t, 120, 40)
	s := fixture()
	feed(m, s)
	press(m, "enter")
	for _, tc := range []struct {
		key  model.RowKey
		want []string
	}{
		{model.RowKey{ContainerID: "4e5d6c7b8a90"}, []string{"| no process holds the port (published", "|   by Docker)"}},
		{keyOf(s, 0), []string{"| the process holding this port could", "|   not be read"}},
		{model.RowKey{Header: model.GroupOther}, []string{"| processes outside every project"}},
	} {
		detailSelect(t, m, tc.key)
		for _, want := range tc.want {
			if got := ansi.Cut(line(m, want), 80, 120); got != want {
				t.Errorf("row %+v: pane line %q, want %q\n%s", tc.key, got, want, screen(m))
			}
		}
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
	if !warned(t, m, "hint      run", "run with sudo to see owners") {
		t.Error("the hint is not in the warning colour (DEV-108)")
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

// longArgv is a Java command line of over 3,000 characters: one classpath word of 60 jars
// between a few flags, the shape that used to fill the pane (DEV-86).
func longArgv() []string {
	jars := make([]string, 60)
	for i := range jars {
		jars[i] = fmt.Sprintf("/home/me/.gradle/caches/modules-2/jars/lib-%02d-1.0.jar", i)
	}
	argv := []string{"java", "-Xmx2g", "-cp", strings.Join(jars, ":"), "com.example.Main", "--port", "5173"}
	if n := len(strings.Join(argv, " ")); n < 3000 {
		panic(fmt.Sprintf("longArgv is %d characters, want at least 3000", n))
	}
	return argv
}

// paneLines returns the detail pane's lines without the split's separator: the right
// columns of the body at 120 columns or more, the whole body below.
func paneLines(m *Model) []string {
	body := bodyLines(m)
	if m.width < splitWidth {
		return body
	}
	dw := detailWidth(m.width)
	out := make([]string, len(body))
	for i, l := range body {
		p := ansi.Cut(l, m.width-dw, m.width)
		out[i] = strings.TrimRight(strings.TrimPrefix(strings.TrimPrefix(p, "|"), " "), " ")
	}
	return out
}

// panePosition parses the pane's position line (its last line) as the 1-based first and last
// document lines shown and the document's length; ok is false when there is none.
func panePosition(m *Model) (first, last, n int, ok bool) {
	pl := paneLines(m)
	_, err := fmt.Sscanf(pl[len(pl)-1], "lines %d-%d of %d", &first, &last, &n)
	return first, last, n, err == nil
}

// paneDocument pages through the pane with pgdown from where it is to its end and returns
// the document lines seen, by index (lines above the starting point stay empty).
func paneDocument(t *testing.T, m *Model) []string {
	t.Helper()
	var doc []string
	for range 200 {
		first, last, n, ok := panePosition(m)
		if !ok {
			t.Fatalf("no position line on an overflowing pane:\n%s", screen(m))
		}
		if doc == nil {
			doc = make([]string, n)
		}
		for i, l := range paneLines(m)[:last-first+1] {
			doc[first-1+i] = l
		}
		if last == n {
			return doc
		}
		scroll(m, "pgdown")
	}
	t.Fatal("pgdown never reaches the end of the pane")
	return nil
}

func TestDetailLongArgv(t *testing.T) {
	argv := longArgv()
	for _, tc := range []struct{ w, h int }{{120, 40}, {80, 24}} {
		t.Run(fmt.Sprintf("%dx%d", tc.w, tc.h), func(t *testing.T) {
			m, _ := newTest(t, tc.w, tc.h)
			s := fixture()
			s.Processes[1].Argv = argv
			feed(m, s)
			k := keyOf(s, 101)
			detailSelect(t, m, k)
			press(m, "enter")

			// At the top: the command is capped at a third of the pane and ends with the count
			// of lines it hides; every field after it is on screen.
			pl := paneLines(m)
			if !strings.HasPrefix(pl[0], "node 101 · server") {
				t.Fatalf("pane starts with %q, want the title:\n%s", pl[0], screen(m))
			}
			cmd := 0
			for _, l := range pl[1:] {
				if !strings.HasPrefix(l, "command ") && (cmd == 0 || !strings.HasPrefix(l, "          ")) {
					break
				}
				cmd++
			}
			if limit := len(pl) / 3; cmd > limit || cmd < 2 {
				t.Errorf("command takes %d lines, want 2 to %d (a third of the %d-line pane)", cmd, limit, len(pl))
			}
			if l := strings.TrimSpace(pl[cmd]); !strings.HasPrefix(l, "… +") || !strings.HasSuffix(l, " lines") {
				t.Errorf("capped command ends with %q, want \"… +N lines\"", l)
			}
			for _, label := range []string{"cwd", "project", "listeners", "parents", "started", "user"} {
				if !slices.ContainsFunc(pl, func(l string) bool { return strings.HasPrefix(l, label+" ") }) {
					t.Errorf("%s is not on screen under the long argv:\n%s", label, screen(m))
				}
			}

			// The pane scrolls; pgdown leaves the selection alone, and the full argv is at the end.
			doc := paneDocument(t, m)
			if m.sel != k {
				t.Errorf("pgdown moved the selection to %+v while the pane was open", m.sel)
			}
			full := slices.IndexFunc(doc, func(l string) bool { return strings.HasPrefix(l, "full argv ") })
			if full < 0 {
				t.Fatalf("no full argv in the pane document:\n%s", strings.Join(doc, "\n"))
			}
			var got strings.Builder
			for i, l := range doc[full:] {
				if i == 0 {
					l = strings.TrimPrefix(l, "full argv")
				}
				got.WriteString(strings.ReplaceAll(l, " ", ""))
			}
			if want := strings.Join(argv, ""); got.String() != want {
				t.Errorf("full argv in the pane:\n%s\nwant\n%s", got.String(), want)
			}

			// pgup goes back to the top.
			for range 50 {
				scroll(m, "pgup")
			}
			if first, _, _, _ := panePosition(m); first != 1 || !strings.HasPrefix(paneLines(m)[0], "node 101") {
				t.Errorf("pgup does not return to the top:\n%s", screen(m))
			}
		})
	}
}

func TestDetailScrollResets(t *testing.T) {
	for _, w := range []int{80, 120} {
		m, _ := newTest(t, w, 24)
		s := fixture()
		s.Processes[1].Argv = longArgv()
		s.Processes[2].Argv = longArgv()
		feed(m, s)
		detailSelect(t, m, keyOf(s, 101))
		press(m, "enter")
		scroll(m, "pgdown")
		if first, _, _, ok := panePosition(m); !ok || first == 1 {
			t.Fatalf("width %d: pgdown does not scroll the pane:\n%s", w, screen(m))
		}
		// Another row starts at the top.
		press(m, "down")
		if first, _, _, _ := panePosition(m); first != 1 || !strings.HasPrefix(paneLines(m)[0], "esbuild 102") {
			t.Errorf("width %d: a new selection does not start at the top:\n%s", w, screen(m))
		}
		// So does the pane after closing and opening it again.
		scroll(m, "pgdown", "enter", "enter")
		if first, _, _, _ := panePosition(m); first != 1 {
			t.Errorf("width %d: enter, enter does not start the pane at the top:\n%s", w, screen(m))
		}
		scroll(m, "pgdown", "esc", "enter")
		if first, _, _, _ := panePosition(m); first != 1 {
			t.Errorf("width %d: esc, enter does not start the pane at the top:\n%s", w, screen(m))
		}
		// A pane that fits has no position line, and pgdown does not move it.
		detailSelect(t, m, keyOf(s, 200))
		scroll(m, "pgdown")
		if _, _, _, ok := panePosition(m); ok || !strings.HasPrefix(paneLines(m)[0], "api 200") {
			t.Errorf("width %d: a short pane scrolls or shows a position line:\n%s", w, screen(m))
		}
		if strings.Contains(screen(m), "full argv") || strings.Contains(screen(m), "… +") {
			t.Errorf("width %d: a short command is capped:\n%s", w, screen(m))
		}
	}
}

func TestDetailClosedPageKeysMoveTable(t *testing.T) {
	m, _ := newTest(t, 120, 24)
	s := fixture()
	feed(m, s)
	detailSelect(t, m, keyOf(s, 101))
	before := m.selIdx
	scroll(m, "pgdown")
	if m.selIdx == before {
		t.Error("with the pane closed, pgdown no longer moves the table")
	}
}

func TestDetailTags(t *testing.T) {
	systemd := model.Process{PID: 900, PPID: 1, StartTime: at(99 * time.Hour), UID: 501, Name: "systemd",
		Argv: []string{"/usr/lib/systemd/systemd", "--user"}}
	for _, tc := range []struct {
		name string
		os   string
		ppid int
		want string
	}{
		{"macOS", "darwin", 1, "parent exited; now a child of launchd"},
		{"Linux init", "linux", 1, "parent exited; now a child of init"},
		{"Linux systemd --user", "linux", 900, "parent exited; now a child of the user's systemd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTest(t, 100, 40)
			s := fixture()
			s.Host.OS = tc.os
			s.Processes[4].PPID = tc.ppid
			s.Processes = append(s.Processes, systemd)
			feed(m, s)
			detailSelect(t, m, keyOf(s, 200))
			press(m, "enter")
			hasLine(t, m, "tags      orphaned: "+tc.want)
			hasLine(t, m, "          cwd deleted: working directory deleted")
		})
	}

	// An untagged process has no tags line.
	m, _ := newTest(t, 100, 40)
	s := fixture()
	feed(m, s)
	detailSelect(t, m, keyOf(s, 101))
	press(m, "enter")
	if line(m, "tags") != "" {
		t.Errorf("an untagged process lists tags:\n%s", screen(m))
	}
}
