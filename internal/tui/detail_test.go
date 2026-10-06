package tui

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
)

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
	selectRow(t, m, keyOf(s, 200))
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
	selectRow(t, m, keyOf(s, 101))
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
		selectRow(t, m, keyOf(s, 200))
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

// TestDetailStartedLocalZone: the start time is shown in the machine's zone, time.Local, whatever
// zone the snapshot's time carries, and the tests' pinned zone (TestMain) is what keeps the
// detail goldens the same on every machine (DEV-90). The snapshot's zone varies, not time.Local:
// a tick timer a Run test leaves behind reads time.Local, so no test writes it (DEV-199).
func TestDetailStartedLocalZone(t *testing.T) {
	if time.Local != time.UTC {
		t.Fatalf("time.Local is %v, want UTC pinned by TestMain so the goldens do not depend on TZ", time.Local)
	}
	for _, zone := range []*time.Location{time.UTC, time.FixedZone("UTC+3", 3*60*60), time.FixedZone("UTC-7", -7*60*60)} {
		t.Run(zone.String(), func(t *testing.T) {
			m, _ := newTest(t, 100, 40)
			s := fixture()
			s.Processes[1].StartTime = s.Processes[1].StartTime.In(zone) // node vite, 101
			feed(m, s)
			selectRow(t, m, keyOf(s, 101))
			press(m, "enter")
			hasLine(t, m, "started   2026-10-02 09:00:00 (3 h ago)")
		})
	}
}

func TestDetailProcess(t *testing.T) {
	m, _ := newTest(t, 100, 40)
	s := fixture()
	feed(m, s)
	selectRow(t, m, keyOf(s, 101))
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
	selectRow(t, m, keyOf(s, 200))
	hasLine(t, m, "listeners tcp4 127.0.0.1:8080")
	hasLine(t, m, "          tcp6 [::1]:8081")
	hasLine(t, m, "parents   sshd 1")
	selectRow(t, m, keyOf(s, 103))
	hasLine(t, m, "cpu       –")
	hasLine(t, m, "listeners none")
	openOther(m)
	selectRow(t, m, keyOf(s, 1))
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
			openOther(m) // no project: every process is in other
			selectRow(t, m, keyOf(s, 101))
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
			selectRow(t, m, keyOf(s, 300))
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
			selectRow(t, m, model.RowKey{ContainerID: "4e5d6c7b8a90"})
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
			selectRow(t, m, keyOf(s, 200))
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
	selectRow(t, m, keyOf(s, 102))
	press(m, "enter")
	// The value column is 40 wide: the path is cut at 40 cells, the flags fill the next line.
	hasLine(t, m, "command   /src/shop/node_modules/@esbuild/darwin-a")
	hasLine(t, m, "          rm64/bin/esbuild --service=0.21.5 --ping")

	// Word wrapping keeps whole arguments together.
	s.Processes[1].Argv = []string{"node", "node_modules/.bin/vite", "--port", "5173", "--host", "0.0.0.0", "--strictPort"}
	feed(m, s)
	selectRow(t, m, keyOf(s, 101))
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
	selectRow(t, m, keyOf(s, 200))
	press(m, "enter")
	if line(m, "command") == "" {
		t.Errorf("the 11-column overlay does not show the command field:\n%s", screen(m))
	}
}

// TestDetailContainerForwarder is DEV-180: OrbStack Helper holds two containers' ports, so it
// keeps its own row and the container rows have no process; the pane names it as the holder of
// the container's port, as `port N` does, instead of saying no process holds it.
func TestDetailContainerForwarder(t *testing.T) {
	m, _ := newTest(t, 100, 40)
	s := fixture()
	s.Containers = append(s.Containers, model.Container{ID: "aaa", Name: "qa-api", Image: "nginx:alpine", State: "running",
		Ports: []model.PortMapping{{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: 6000, ContainerPort: 80, Proto: "tcp"}}})
	helper := model.Process{PID: 400, PPID: 1, StartTime: at(time.Hour), UID: 501, Name: "OrbStack Helper", Argv: []string{"OrbStack Helper"},
		Listeners: []model.Listener{lis("tcp6", "::", 8000), lis("tcp6", "::", 6000)}}
	s.Processes = model.Reconcile(append(s.Processes, helper), s.Containers)
	feed(m, s)
	press(m, "enter")
	for _, id := range []string{"4e5d6c7b8a90", "aaa"} {
		selectRow(t, m, model.RowKey{ContainerID: id})
		hasLine(t, m, "held by OrbStack Helper 400 (forwards the container's port)")
		if strings.Contains(screen(m), "no process holds the port") {
			t.Errorf("%s: forwarded port reads as held by no process:\n%s", id, screen(m))
		}
	}
}

// TestDetailContainerUnheldPort is DEV-186: a container publishes 6000, which OrbStack Helper
// forwards, and 6001 (on both families), which no host socket holds; the pane names the
// forwarder and says, once, that nothing holds 6001.
func TestDetailContainerUnheldPort(t *testing.T) {
	m, _ := newTest(t, 100, 40)
	s := fixture()
	s.Containers = append(s.Containers, model.Container{ID: "aaa", Name: "qa-api", Image: "nginx:alpine", State: "running",
		Ports: []model.PortMapping{
			{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: 6000, ContainerPort: 80, Proto: "tcp"},
			{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: 6001, ContainerPort: 81, Proto: "tcp"},
			{HostIP: netip.MustParseAddr("::"), HostPort: 6001, ContainerPort: 81, Proto: "tcp"},
			{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: 6002, ContainerPort: 82, Proto: "udp"},
		}})
	helper := model.Process{PID: 400, PPID: 1, StartTime: at(time.Hour), UID: 501, Name: "OrbStack Helper", Argv: []string{"OrbStack Helper"},
		Listeners: []model.Listener{lis("tcp6", "::", 8000), lis("tcp6", "::", 6000)}}
	s.Processes = model.Reconcile(append(s.Processes, helper), s.Containers)
	feed(m, s)
	press(m, "enter")
	selectRow(t, m, model.RowKey{ContainerID: "aaa"})
	hasLine(t, m, "held by OrbStack Helper 400 (forwards the container's port)")
	hasLine(t, m, "no process holds port 6001 (published by Docker)")
	if n := strings.Count(screen(m), "no process holds"); n != 1 {
		t.Errorf("want one no-holder line (6001; 6002 is udp), got %d:\n%s", n, screen(m))
	}
}

// TestDetailProcessRowUnheldPort is DEV-196: shop-db-1 publishes 5432, which its docker-proxy
// holds, and 5433 (on both families), which no host socket holds; it is drawn as the proxy's
// row (or the PID 0 owner's when the proxy is root's), whose pane says, once, that nothing
// holds 5433, and names no holder, since the row is the holder.
func TestDetailProcessRowUnheldPort(t *testing.T) {
	for name, s := range map[string]model.Snapshot{"proxy": fixture(), "unknown owner": hiddenProxy()} {
		t.Run(name, func(t *testing.T) {
			m, _ := newTest(t, 100, 40)
			s.Containers[0].Ports = append(s.Containers[0].Ports,
				model.PortMapping{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: 5433, ContainerPort: 5433, Proto: "tcp"},
				model.PortMapping{HostIP: netip.MustParseAddr("::"), HostPort: 5433, ContainerPort: 5433, Proto: "tcp"},
				model.PortMapping{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: 5434, ContainerPort: 5434, Proto: "udp"})
			feed(m, s)
			press(m, "enter")
			selectRow(t, m, s.Processes[slices.IndexFunc(s.Processes, func(p model.Process) bool { return p.ContainerID != "" })].Key())
			hasLine(t, m, "no process holds port 5433 (published by Docker)")
			body := strings.Join(bodyLines(m), "\n")
			if n := strings.Count(body, "no process holds"); n != 1 {
				t.Errorf("want one no-holder line (5433; 5434 is udp), got %d:\n%s", n, screen(m))
			}
			if strings.Contains(body, "held by") {
				t.Errorf("the row's own process is named as a holder:\n%s", screen(m))
			}
		})
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
			selectRow(t, m, s.Processes[len(s.Processes)-1].Key())
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
			selectRow(t, m, s.Processes[len(s.Processes)-1].Key())
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
	openOther(m)
	press(m, "enter")
	for _, tc := range []struct {
		key  model.RowKey
		want []string
	}{
		{model.RowKey{ContainerID: "4e5d6c7b8a90"}, []string{"| no process holds the port (published", "|   by Docker)"}},
		{keyOf(s, 0), []string{"| the process holding this port could", "|   not be read"}},
		{model.RowKey{Header: model.GroupOther}, []string{"| processes outside every project"}},
	} {
		selectRow(t, m, tc.key)
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

	selectRow(t, m, model.RowKey{Header: model.GroupProject, Group: shopID})
	for _, want := range []string{"shop @ feat/cart (worktree)", "root      /src/shop", "branch    feat/cart",
		"main repo /src/shop-main"} {
		hasLine(t, m, want)
	}
	selectRow(t, m, model.RowKey{Header: model.GroupProject, Group: apiID})
	hasLine(t, m, "api @ abc1234")
	hasLine(t, m, "branch    detached at abc1234")
	if strings.Contains(screen(m), "main repo") {
		t.Error("main repo shown for a project that is not a worktree")
	}
	// Neither branch nor SHA (a reftable repository, an unreadable HEAD): no branch field, like
	// the title (DEV-163).
	s.Projects[1].ShortSHA = ""
	feed(m, s)
	selectRow(t, m, model.RowKey{Header: model.GroupProject, Group: apiID})
	hasLine(t, m, "api")
	if strings.Contains(screen(m), "branch") {
		t.Errorf("branch field shown for an unknown branch:\n%s", screen(m))
	}
	selectRow(t, m, model.RowKey{Header: model.GroupCompose, Group: "shop"})
	hasLine(t, m, "shop (compose)")
	hasLine(t, m, "members   shop-db-1, shop-web-1")
	selectRow(t, m, model.RowKey{Header: model.GroupOther})
	hasLine(t, m, "other")
	hasLine(t, m, "processes outside every project")
}

func TestDetailUnknownOwner(t *testing.T) {
	m, _ := newTest(t, 100, 30)
	s := fixture()
	feed(m, s)
	openOther(m)
	selectRow(t, m, keyOf(s, 0))
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
		press(m, "pgdown")
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
			selectRow(t, m, k)
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
				press(m, "pgup")
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
		selectRow(t, m, keyOf(s, 101))
		press(m, "enter")
		press(m, "pgdown")
		if first, _, _, ok := panePosition(m); !ok || first == 1 {
			t.Fatalf("width %d: pgdown does not scroll the pane:\n%s", w, screen(m))
		}
		// Another row starts at the top.
		press(m, "down")
		if first, _, _, _ := panePosition(m); first != 1 || !strings.HasPrefix(paneLines(m)[0], "esbuild 102") {
			t.Errorf("width %d: a new selection does not start at the top:\n%s", w, screen(m))
		}
		// So does the pane after closing and opening it again.
		press(m, "pgdown", "enter", "enter")
		if first, _, _, _ := panePosition(m); first != 1 {
			t.Errorf("width %d: enter, enter does not start the pane at the top:\n%s", w, screen(m))
		}
		press(m, "pgdown", "esc", "enter")
		if first, _, _, _ := panePosition(m); first != 1 {
			t.Errorf("width %d: esc, enter does not start the pane at the top:\n%s", w, screen(m))
		}
		// A pane that fits has no position line, and pgdown does not move it.
		selectRow(t, m, keyOf(s, 200))
		press(m, "pgdown")
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
	selectRow(t, m, keyOf(s, 101))
	before := m.selIdx
	press(m, "pgdown")
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
			selectRow(t, m, keyOf(s, 200))
			press(m, "enter")
			hasLine(t, m, "tags      orphaned: "+tc.want)
			hasLine(t, m, "          cwd deleted: working directory deleted")
		})
	}

	// An untagged process has no tags line.
	m, _ := newTest(t, 100, 40)
	s := fixture()
	feed(m, s)
	selectRow(t, m, keyOf(s, 101))
	press(m, "enter")
	if line(m, "tags") != "" {
		t.Errorf("an untagged process lists tags:\n%s", screen(m))
	}
}

// TestDetailLocation: a process's project ends with where it is from devdash's own repository,
// as the port answer says it: this repo, another worktree of it, or nothing.
func TestDetailLocation(t *testing.T) {
	s := fixture()
	m := newPortTest(t, 100, 40, s, &fakeProbe{})
	selectRow(t, m, keyOf(s, 200))
	press(m, "enter")
	hasLine(t, m, "project   api @ main · this repo")
	selectRow(t, m, keyOf(s, 101))
	hasLine(t, m, "project   shop @ feat/cart (worktree)")

	// devdash run from shop's main repository: the feat/cart worktree is another worktree of it.
	s = fixture()
	s.Projects[1].Here = false
	s.Projects = append(s.Projects, model.Project{ID: "/src/shop-main", Root: "/src/shop-main", Name: "shop", Branch: "main", Here: true})
	m = newPortTest(t, 100, 40, s, &fakeProbe{})
	selectRow(t, m, keyOf(s, 101))
	press(m, "enter")
	hasLine(t, m, "project   shop @ feat/cart (worktree) · this repo, other worktree")
	selectRow(t, m, keyOf(s, 200))
	hasLine(t, m, "project   api @ main")

	// No Here project: no marker.
	s = fixture()
	s.Projects[1].Here = false
	m = newPortTest(t, 100, 40, s, &fakeProbe{})
	selectRow(t, m, keyOf(s, 200))
	press(m, "enter")
	hasLine(t, m, "project   api @ main")
}

// openDetail selects the row with key k with the pane closed, opens the pane and returns the
// command enter returned.
func openDetail(t *testing.T, m *Model, k model.RowKey) tea.Cmd {
	t.Helper()
	m.closeDetail()
	selectRow(t, m, k)
	return press(m, "enter")
}

// TestDetailProjectTitle: the title follows the LabelParts rule with the name and the ref
// quoted apart, so an unprintable byte in one leaves the rest of the title as it is.
func TestDetailProjectTitle(t *testing.T) {
	for _, tt := range []struct {
		pr   model.Project
		want string
	}{
		{model.Project{Name: "api", Branch: "main"}, "api @ main"},
		{model.Project{Name: "api", ShortSHA: "abc1234", Worktree: true}, "api @ abc1234 (worktree)"},
		{model.Project{Name: "api"}, "api"},
		{model.Project{Name: "a\x1bpi", Branch: "fe\x9bat", Worktree: true}, `"a\x1bpi" @ "fe\x9bat" (worktree)`},
	} {
		if got := detailProjectTitle(&tt.pr); got != tt.want {
			t.Errorf("detailProjectTitle(%+v) = %q, want %q", tt.pr, got, tt.want)
		}
	}
}

// TestDetailNextFree: a process with a listener gets a next free field after its listeners,
// the search from its lowest TCP port plus one, `…` until the answer arrives.
func TestDetailNextFree(t *testing.T) {
	s := fixture()
	fp := &fakeProbe{}
	m := newPortTest(t, 100, 40, s, fp)
	cmd := openDetail(t, m, keyOf(s, 200))
	if len(fp.calls) != 0 {
		t.Fatalf("probed on the UI goroutine: %v", fp.calls)
	}
	pane := strings.Join(paneLines(m), "\n")
	if !strings.Contains(pane, "listeners tcp4 127.0.0.1:8080\n          tcp6 [::1]:8081\nnext free …\nparents") {
		t.Errorf("next free is not … after the listeners:\n%s", pane)
	}
	runAll(m, cmd)
	hasLine(t, m, "next free 8082") // 8081 is api's own, from the snapshot: never probed
	if !slices.Equal(fp.calls, []uint16{8082}) {
		t.Errorf("probe calls %v, want 8082", fp.calls)
	}

	errMFILE := errors.New("socket: too many\x1bopen files")
	for _, c := range []struct {
		name  string
		probe *fakeProbe
		pid   int
		want  string
	}{
		{"nothing free", (&fakeProbe{}).takeRange(8082, 8180), 200, "next free none in 8081-8180"},
		{"probe failed", &fakeProbe{err: errMFILE}, 200, "next free socket: too many?open files"},
		{"one listener", &fakeProbe{}, 101, "next free 5174"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newPortTest(t, 100, 40, s, c.probe)
			runAll(m, openDetail(t, m, keyOf(s, c.pid)))
			hasLine(t, m, c.want)
			if c.probe.err != nil && !warned(t, m, "next free", "socket: too many?open files") {
				t.Errorf("the error is not in the warning colour:\n%q", styled(t, m, colorprofile.ANSI))
			}
		})
	}

	// The lowest TCP port, whatever the listeners' order; UDP does not count.
	s2 := withProcess(fixture(), model.KindServer, 0)
	s2.Processes[len(s2.Processes)-1].Listeners = []model.Listener{lis("tcp4", "0.0.0.0", 9100), lis("udp4", "0.0.0.0", 53),
		lis("tcp6", "::", 9000)}
	m = newPortTest(t, 100, 40, s2, &fakeProbe{})
	runAll(m, openDetail(t, m, keyOf(s2, 204)))
	hasLine(t, m, "next free 9001")

	// No field and no probe: no listener, UDP only, a lowest port of 65535.
	for name, ls := range map[string][]model.Listener{
		"no listener": nil,
		"UDP only":    {lis("udp4", "0.0.0.0", 53)},
		"65535":       {lis("tcp4", "0.0.0.0", 65535)},
	} {
		s3 := withProcess(fixture(), model.KindServer, 0)
		s3.Processes[len(s3.Processes)-1].Listeners = ls
		fp := &fakeProbe{}
		m := newPortTest(t, 100, 40, s3, fp)
		if cmd := openDetail(t, m, keyOf(s3, 204)); cmd != nil {
			runAll(m, cmd)
			t.Errorf("%s: opening the pane returned a command (probe calls %v)", name, fp.calls)
		}
		if l := line(m, "next free"); l != "" {
			t.Errorf("%s: %q", name, l)
		}
	}
}

// TestDetailNextFreeRuns: the probe runs off the UI goroutine when the pane shows a row it has
// no answer for and on each new snapshot while it is open; answers for another row or an
// older snapshot are dropped, and the row's last answer stays until the next one arrives.
func TestDetailNextFreeRuns(t *testing.T) {
	s := fixture()
	fp := &fakeProbe{}
	m := newPortTest(t, 100, 40, s, fp)
	refresh := func(s model.Snapshot, ago time.Duration) tea.Cmd {
		s.TakenAt = at(ago)
		_, cmd := m.Update(updateMsg(engine.Update{Snapshot: s, Interval: 2 * time.Second}))
		return cmd
	}

	// Pane closed: moving and refreshing never probe.
	rowsSelect(t, m, keyOf(s, 200))
	runAll(m, refresh(s, time.Second))
	if len(fp.calls) != 0 {
		t.Fatalf("probed with the pane closed: %v", fp.calls)
	}

	// Open on api, move to go test (no listener) and on to vite before api's answer arrives.
	apiAns := press(m, "enter")
	if cmd := press(m, "down"); cmd != nil {
		t.Error("go test, without a listener, returned a command")
	}
	var viteAns tea.Cmd
	for range 10 {
		if viteAns = press(m, "down"); m.sel == keyOf(s, 101) {
			break
		}
	}
	if m.sel != keyOf(s, 101) || viteAns == nil {
		t.Fatalf("vite selected %v, command %v", m.sel == keyOf(s, 101), viteAns != nil)
	}
	runAll(m, apiAns)
	hasLine(t, m, "next free …")
	runAll(m, viteAns)
	hasLine(t, m, "next free 5174")

	// A key that changes nothing shown asks for nothing.
	if cmd := press(m, "pgdown"); cmd != nil {
		t.Error("a key on the same row and snapshot returned a command")
	}

	// A new snapshot: the last answer stays until the new one arrives; an older one is dropped.
	newer := withProcess(fixture(), model.KindServer, 5174)
	stale := refresh(fixture(), 500*time.Millisecond)
	fresh := refresh(newer, 0)
	hasLine(t, m, "next free 5174")
	runAll(m, fresh)
	hasLine(t, m, "next free 5175")
	runAll(m, stale)
	hasLine(t, m, "next free 5175")

	// The same snapshot again (a failed tick keeps the last good one) is not new.
	calls := len(fp.calls)
	_, cmd := m.Update(updateMsg(engine.Update{Snapshot: m.upd.Snapshot, Err: errors.New("timeout"), Interval: 2 * time.Second}))
	runAll(m, cmd)
	if len(fp.calls) != calls {
		t.Errorf("the same snapshot probed again: %v", fp.calls[calls:])
	}

	// Closed: a new snapshot does not probe; opening again on it does, and the row's last
	// answer shows until the new one arrives.
	press(m, "esc")
	runAll(m, refresh(newer, -time.Second))
	if len(fp.calls) != calls {
		t.Errorf("probed with the pane closed: %v", fp.calls[calls:])
	}
	_, cmd = m.Update(key("enter"))
	if cmd == nil {
		t.Fatal("reopening on a new snapshot returned no command")
	}
	hasLine(t, m, "next free 5175")
	runAll(m, cmd)
	hasLine(t, m, "next free 5175")
	if len(fp.calls) == calls {
		t.Error("reopening did not probe")
	}
}
