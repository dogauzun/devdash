package tui

import (
	"math"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
)

// Shared test helpers. Feature tests build on fixture() and change copies of it.

// now is the fixed clock of every test; fixture snapshots are taken 2 s before it.
var now = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

// TestMain pins the local zone to UTC: the detail pane shows start times in local time, and
// the goldens and expected lines must not depend on the TZ of the machine running them (DEV-90).
// Nothing writes it after this, not even to restore it: a tick timer a Run test left behind may
// still read it (a race under -race) (DEV-177, DEV-199).
func TestMain(m *testing.M) {
	time.Local = time.UTC
	os.Exit(m.Run())
}

// fakeSource is a Source whose updates the test sends directly through Update, so the
// channel is only there to satisfy Init.
type fakeSource struct {
	ch        chan engine.Update
	refreshes int
}

func (f *fakeSource) Updates() <-chan engine.Update { return f.ch }
func (f *fakeSource) Refresh()                      { f.refreshes++ }

// newTest returns a model at w by h with a fixed clock, a fake source and recording fakes for
// plan, kill and open, and a probe that fails the test when called. Tests override o's fields through mod.
func newTest(t *testing.T, w, h int, mod ...func(*Options)) (*Model, *fakeSource) {
	t.Helper()
	src := &fakeSource{ch: make(chan engine.Update)}
	o := Options{
		Source: src,
		Now:    func() time.Time { return now },
		Plan: func(model.Snapshot, model.RowKey, engine.KillOptions) (engine.Plan, error) {
			t.Error("unexpected Plan call")
			return engine.Plan{}, nil
		},
		Kill: func(engine.Plan, time.Duration) (engine.Result, error) {
			t.Error("unexpected Kill call: tests never signal")
			return engine.Result{}, nil
		},
		Open: func(string) error { t.Error("unexpected Open call"); return nil },
		Probe: func(p uint16) (bool, error) {
			t.Errorf("unexpected Probe call for port %d: TUI tests never bind", p)
			return false, nil
		},
	}
	for _, f := range mod {
		f(&o)
	}
	m := New(o)
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m, src
}

// feed delivers a good snapshot as the engine would.
func feed(m *Model, s model.Snapshot) {
	m.Update(updateMsg(engine.Update{Snapshot: s, Interval: 2 * time.Second}))
}

// openOther unfolds the other group, which starts collapsed (Release 1.1), as → on its header
// would, for tests that select or draw the rows inside it.
func openOther(m *Model) {
	delete(m.view.Collapsed, model.RowKey{Header: model.GroupOther})
	m.rebuild()
}

// press sends key presses: single characters ("j", "/", "?"), or names ("up", "down",
// "left", "right", "enter", "esc", "backspace", "ctrl+c"). It returns the last command.
func press(m *Model, keys ...string) tea.Cmd {
	var cmd tea.Cmd
	for _, k := range keys {
		_, cmd = m.Update(key(k))
	}
	return cmd
}

// typeText sends each rune of s as a key press.
func typeText(m *Model, s string) {
	for _, r := range s {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func key(k string) tea.KeyPressMsg {
	switch k {
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	}
	r := []rune(k)
	if len(r) != 1 {
		panic("press: unknown key " + k)
	}
	return tea.KeyPressMsg{Code: r[0], Text: k}
}

// screen is the rendered view without ANSI styles, with trailing spaces trimmed per line.
func screen(m *Model) string {
	lines := strings.Split(ansi.Strip(m.View().Content), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(lines, "\n")
}

// warned reports whether s is drawn in the warning colour (yellow, SGR 33) on the first line
// that contains at, in the view as the program writes it with colour on.
func warned(t *testing.T, m *Model, at, s string) bool {
	t.Helper()
	for l := range strings.SplitSeq(styled(t, m, colorprofile.ANSI), "\n") {
		if strings.Contains(ansi.Strip(l), at) {
			return strings.Contains(l, "\x1b[33m"+s)
		}
	}
	return false
}

// line returns the first screen line containing s, or "" when none does.
func line(m *Model, s string) string {
	for l := range strings.SplitSeq(screen(m), "\n") {
		if strings.Contains(l, s) {
			return l
		}
	}
	return ""
}

// Fixture: two projects, a compose project, and the `other` group.
//
//	shop @ feat/cart (worktree)       /src/shop
//	  zsh 100 (shell, hidden, dimmed: its child is visible)
//	    node vite 101 (server, *:5173, 3 h)
//	      esbuild 102
//	  claude 103 (agent)
//	api @ main (here)                 /src/api, the project devdash was run from
//	  api 200 (server, 127.0.0.1:8080 and [::1]:8081; orphaned, cwd deleted)
//	  go test 201 (test)
//	  nvim 202 (editor, hidden)
//	shop (compose)
//	  docker-proxy 300 holding shop-db-1's 5432 (container)
//	  shop-web-1, published 8000, no process behind it
//	other
//	  sshd 1 root (*:22)
//	  unknown owner (PID 0, 0.0.0.0:631)
const (
	shopID = "/src/shop"
	apiID  = "/src/api"
)

func at(ago time.Duration) time.Time { return now.Add(-ago) }

func lis(proto, addr string, port uint16) model.Listener {
	return model.Listener{Proto: proto, Addr: netip.MustParseAddr(addr), Port: port}
}

func fixture() model.Snapshot {
	proc := func(pid, ppid int, start time.Time, user, name, cwd, project string, kind model.Kind, argv ...string) model.Process {
		return model.Process{PID: pid, PPID: ppid, StartTime: start, UID: 501, User: user, Name: name,
			Argv: argv, Cwd: cwd, CPUPercent: 0.5, RSSBytes: 50 << 20, Kind: kind, ProjectID: project}
	}
	zsh := proc(100, 90, at(5*time.Hour), "me", "zsh", shopID, shopID, model.KindShell, "-zsh")
	vite := proc(101, 100, at(3*time.Hour), "me", "node", shopID, shopID, model.KindServer,
		"node", "node_modules/.bin/vite", "--port", "5173")
	vite.Listeners = []model.Listener{lis("tcp6", "::", 5173)}
	vite.CPUPercent, vite.RSSBytes = 12.3, 187563008
	esbuild := proc(102, 101, at(3*time.Hour-time.Second), "me", "esbuild", shopID, shopID, model.KindOther,
		"/src/shop/node_modules/@esbuild/darwin-arm64/bin/esbuild", "--service=0.21.5", "--ping")
	claude := proc(103, 90, at(20*time.Minute), "me", "claude", shopID+"/web", shopID, model.KindAgent, "claude")
	claude.CPUPercent = math.NaN() // first sample

	api := proc(200, 1, at(26*time.Hour), "me", "api", apiID, apiID, model.KindServer, "/src/api/bin/api", "-addr", ":8080")
	api.Tags = model.TagOrphaned | model.TagCwdDeleted
	api.Listeners = []model.Listener{lis("tcp4", "127.0.0.1", 8080), lis("tcp6", "::1", 8081)}
	gotest := proc(201, 90, at(30*time.Second), "me", "go", apiID, apiID, model.KindTest, "go", "test", "./...")
	nvim := proc(202, 90, at(2*time.Hour), "me", "nvim", apiID, apiID, model.KindEditor, "nvim", "main.go")

	proxy := proc(300, 1, at(4*time.Hour), "root", "docker-proxy", "", "", model.KindContainer,
		"/usr/bin/docker-proxy", "-proto", "tcp", "-host-ip", "0.0.0.0", "-host-port", "5432")
	proxy.UID, proxy.Unknown = 0, model.FieldCwd|model.FieldCPU|model.FieldMem
	proxy.Listeners = []model.Listener{lis("tcp4", "0.0.0.0", 5432)}
	proxy.Listeners[0].ContainerID = "9f1c2a7b0d3e" // Reconcile marks the socket as the container's
	proxy.ContainerID = "9f1c2a7b0d3e"

	sshd := proc(1, 0, at(72*time.Hour), "root", "sshd", "", "", model.KindServer, "/usr/sbin/sshd", "-D")
	sshd.UID, sshd.Unknown = 0, model.FieldCwd
	sshd.Listeners = []model.Listener{lis("tcp4", "0.0.0.0", 22)}
	unknown := model.Process{Name: "unknown", Listeners: []model.Listener{lis("tcp4", "0.0.0.0", 631)},
		Unknown: model.FieldOwner | model.FieldArgv | model.FieldCwd | model.FieldCPU | model.FieldMem, CPUPercent: math.NaN()}

	return model.Snapshot{
		SchemaVersion: 1,
		TakenAt:       at(2 * time.Second),
		Host:          model.Host{OS: "darwin", Arch: "arm64", Hostname: "mbp", UID: 501},
		Processes:     []model.Process{zsh, vite, esbuild, claude, api, gotest, nvim, proxy, sshd, unknown},
		Projects: []model.Project{
			{ID: shopID, Root: shopID, Name: "shop", Branch: "feat/cart", Worktree: true, MainRepo: "/src/shop-main"},
			{ID: apiID, Root: apiID, Name: "api", Branch: "main", Here: true},
		},
		Containers: []model.Container{
			{ID: "9f1c2a7b0d3e", Name: "shop-db-1", Image: "postgres:16", State: "running", ComposeProject: "shop",
				ComposeService: "db", Ports: []model.PortMapping{{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: 5432, ContainerPort: 5432, Proto: "tcp"}}},
			{ID: "4e5d6c7b8a90", Name: "shop-web-1", Image: "nginx:1.27", State: "running", ComposeProject: "shop",
				ComposeService: "web", Ports: []model.PortMapping{{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: 8000, ContainerPort: 80, Proto: "tcp"}}},
		},
		Warnings: []model.Warning{{Code: "listener_owner_unreadable", Count: 1, Hint: "run with sudo to see owners"}},
	}
}

// keyOf returns the row key of the fixture process with pid.
func keyOf(s model.Snapshot, pid int) model.RowKey {
	for _, p := range s.Processes {
		if p.PID == pid {
			return p.Key()
		}
	}
	panic("keyOf: no such pid")
}
