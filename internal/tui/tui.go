// Package tui is the dashboard: a Bubble Tea v2 program that renders the engine's snapshots as
// one screen (header, tree table grouped by project, footer) with a detail pane, a filter and
// the kill modal on demand. It is a thin client of the engine: everything it shows comes from
// the latest engine.Update, and actions go through the engine's plan and kill functions.
//
// One file per feature: tui.go (state, message routing, layout), header.go (header and footer),
// table.go (rows, columns, movement and view toggles), rows.go (flattening, selection that
// survives a refresh, the filter), port.go (port search and the port line), detail.go, help.go,
// open.go, kill.go and sudo.go (S, rerun under sudo). Each feature file defines its own state type, held in Model, and its own
// messages, which implement action.
package tui

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/freeport"
	"github.com/dogauzun/devdash/internal/model"
)

// Source is the engine as the TUI sees it; *engine.Engine implements it.
type Source interface {
	Updates() <-chan engine.Update // latest wins; closed when the engine stops
	Refresh()                      // non-blocking
}

// Options configures the dashboard. Only Source is required; nil functions get the real
// implementations.
type Options struct {
	Source Source
	// Plan computes what a kill would signal (engine.NewPlan when nil).
	Plan func(model.Snapshot, model.RowKey, engine.KillOptions) (engine.Plan, error)
	// Kill signals a plan and waits up to the timeout ((*engine.Engine).Kill in production,
	// which also asks for a refresh; engine.Kill when nil). It runs off the UI goroutine.
	Kill        func(engine.Plan, time.Duration) (engine.Result, error)
	KillTimeout time.Duration // engine.DefaultKillTimeout when 0
	// Open opens a URL in the browser (open on macOS, xdg-open on Linux, when nil).
	Open func(url string) error
	// Probe reports whether this user can bind a port now, for the port line's next free port
	// (freeport.Probe when nil). It runs off the UI goroutine.
	Probe freeport.Prober
	// DockerSocket returns the Docker endpoint in use, shown in the detail pane of a container
	// row; nil or "" shows nothing.
	DockerSocket func() string
	Now          func() time.Time // time.Now when nil; tests fix it
	ShowAll      bool             // --all: start with shells and editors shown
	// Sudo says S may offer to rerun the dashboard under sudo: devdash is not root and sudo
	// is on PATH. The caller checks both; the TUI never looks at the uid or PATH (sudo.go).
	Sudo bool
}

// Model is the dashboard's state. Use New; the zero value is not ready.
type Model struct {
	o Options

	width, height int

	upd  engine.Update // latest update from the engine
	have bool          // upd holds a good snapshot (SchemaVersion != 0)

	view model.ViewOptions // ShowAll, HideContainers, Sort, Collapsed, Fold, Unfolded (table.go)
	all  []model.Row       // m.upd.Snapshot flattened with m.view; while a filter is set, nothing collapsed or hidden (rebuild)
	rows []model.Row       // all, filtered: what the table shows

	sel    model.RowKey // selected row; never an index (rows.go)
	selIdx int          // index of sel in rows, -1 when rows is empty
	top    int          // first table row on screen (table.go)
	tcache tableCache   // derived from all rows, per rebuild (table.go)

	filter    string // active filter query (rows.go)
	filtering bool   // the filter prompt has the keyboard

	fsel filterSel // the row chosen before the filter hid it (rows.go)
	port portState // the port line while the query is a port number (port.go)

	detail  bool         // detail pane open (detail.go)
	dscroll detailScroll // its scroll position (detail.go)
	dfree   detailFree   // its next free port (detail.go)
	help    bool         // help overlay open (help.go)
	kill    killState
	kafter  killAfter // the last kill's ports, until the first snapshot after it (kill.go)
	hpos    helpPos   // help overlay scroll position (help.go)
	sudo    sudoState // the S confirmation (sudo.go)

	status string // one-shot message in the footer (open failed, kill result); cleared by the next key
}

// action is a message a feature file defines for itself (a finished kill, a failed open);
// Update hands it back to that file.
type action interface{ apply(m *Model) tea.Cmd }

// updateMsg carries one engine.Update; closedMsg says the engine stopped; tickMsg redraws the
// snapshot age once a second.
type (
	updateMsg engine.Update
	closedMsg struct{}
	tickMsg   time.Time
)

// New returns the dashboard model for o.
func New(o Options) *Model {
	if o.Plan == nil {
		o.Plan = engine.NewPlan
	}
	if o.Kill == nil {
		o.Kill = engine.Kill
	}
	if o.KillTimeout == 0 {
		o.KillTimeout = engine.DefaultKillTimeout
	}
	if o.Open == nil {
		o.Open = openURL
	}
	if o.Probe == nil {
		o.Probe = freeport.Probe
	}
	o.Probe = serialProbe(o.Probe)
	if o.Now == nil {
		o.Now = time.Now
	}
	// The other group starts collapsed (Release 1.1): on a Mac it fills with system listeners
	// that push down what the developer started. The fold is not remembered between runs.
	collapsed := map[model.RowKey]bool{{Header: model.GroupOther}: true}
	m := &Model{o: o, selIdx: -1, view: model.ViewOptions{ShowAll: o.ShowAll, Collapsed: collapsed,
		Fold: true, Unfolded: map[model.RowKey]bool{}}}
	m.rebuild()
	return m
}

// serialProbe returns probe with its calls one at a time. The port line's and the detail
// pane's searches run in commands at once (both on each snapshot), and freeport.Probe binds
// the port it is asked: on macOS, without SO_REUSEADDR, two binds of one port collide, so one
// search would skip a free port or read it as refused. Each call closes its sockets before it
// returns, so one call at a time is enough.
func serialProbe(probe freeport.Prober) freeport.Prober {
	var mu sync.Mutex
	return func(port uint16) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		return probe(port)
	}
}

// beforeSignalStop runs in Run after the program returned, just before Run's signal handler is
// removed: a test sends a signal there, in the window DEV-177 closes.
var beforeSignalStop = func() {}

// Run starts the dashboard on the terminal and blocks until the user quits, ctx is done or
// the engine stops. sudo reports that the user confirmed S: the caller reruns devdash under
// sudo, now that the terminal is restored. SIGINT, SIGTERM and SIGHUP quit like ctrl-c, and
// never with sudo, even after y: they cancel ctx in place of Bubble Tea's own handler, which
// knows no SIGHUP and turns SIGTERM into the same quit as y's. A signal that arrives after the
// program returned, until the handler is removed, counts too; one after that meets Go's
// default and ends devdash before any exec.
func Run(ctx context.Context, o Options, opts ...tea.ProgramOption) (sudo bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	returned, handled := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(handled)
		select {
		case <-sigs:
			cancel()
		case <-returned:
		}
	}()
	opts = append([]tea.ProgramOption{tea.WithContext(ctx), tea.WithoutSignalHandler()}, opts...)
	final, err := tea.NewProgram(New(o), opts...).Run()
	beforeSignalStop()
	signal.Stop(sigs) // waits for signals already received to reach sigs
	close(returned)
	<-handled
	if len(sigs) > 0 { // one the goroutine did not take
		cancel()
	}
	if ctx.Err() != nil && (err == nil || errors.Is(err, tea.ErrProgramKilled)) { // bubbletea wraps ctx.Err() into it
		return false, nil
	}
	if m, ok := final.(*Model); ok && err == nil {
		sudo = m.sudo.asked
	}
	return sudo, err
}

// Init implements tea.Model.
func (m *Model) Init() tea.Cmd { return tea.Batch(m.wait(), tick()) }

func (m *Model) wait() tea.Cmd {
	ch := m.o.Source.Updates()
	return func() tea.Msg {
		u, ok := <-ch
		if !ok {
			return closedMsg{}
		}
		return updateMsg(u)
	}
}

func tick() tea.Cmd { return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) }) }

// Update implements tea.Model. After every message, the detail pane asks for the next free
// port of a process it has no answer for (detailProbe).
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.update(msg)
	if probe := m.detailProbe(); probe != nil {
		cmd = tea.Batch(cmd, probe)
	}
	return m, cmd
}

// update handles one message and returns its command.
func (m *Model) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case updateMsg:
		m.upd = engine.Update(msg)
		m.have = m.upd.Snapshot.SchemaVersion != 0
		m.rebuild()
		m.killPorts()
		return tea.Batch(m.wait(), m.portProbe())
	case closedMsg:
		return tea.Quit
	case tickMsg:
		return tick()
	case tea.KeyPressMsg:
		m.status, m.kafter = "", killAfter{} // a key clears the status before the kill's ports are in it
		return m.key(msg)
	case tea.PasteMsg:
		return m.paste(msg.Content)
	case action:
		return msg.apply(m)
	}
	return nil
}

// key routes a key press: ctrl+c always quits; an open sudo confirmation, kill modal, help
// overlay or filter prompt takes every other key; otherwise the global keys, then the table's.
func (m *Model) key(k tea.KeyPressMsg) tea.Cmd {
	s := k.String()
	if s == "ctrl+c" {
		return tea.Quit
	}
	switch {
	case m.sudo.open:
		return m.sudoKey(k)
	case m.kill.active():
		return m.killKey(k)
	case m.help:
		return m.helpKey(k)
	case m.filtering:
		return m.filterKey(k)
	}
	switch s {
	case "q":
		return tea.Quit
	case "r":
		m.o.Source.Refresh()
	case "?":
		m.help = true
	case "/":
		m.filtering = true
	case "enter":
		if m.detail {
			m.closeDetail()
		} else {
			m.detail = true
		}
	case "esc":
		switch {
		case m.detail:
			m.closeDetail()
		case m.filter != "":
			return m.setFilter("")
		}
	case "x":
		return m.startKill()
	case "o":
		return m.openSelected()
	case "S":
		m.sudoStart()
	case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9":
		if k.Mod&(tea.ModCtrl|tea.ModAlt) == 0 { // the prompt would not type it either
			return m.portKey(s)
		}
	default:
		if m.detail && m.detailKey(s) { // pgup and pgdown scroll the open pane
			return nil
		}
		return m.tableKey(k)
	}
	return nil
}

// Layout constants (spec "TUI design").
const (
	minWidth   = 80  // the narrowest width the layout is designed for; narrower still renders
	splitWidth = 120 // the detail pane is a right split from here, a full-screen overlay below
)

// View implements tea.Model.
func (m *Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "devdash"
	return v
}

// render draws the whole screen: exactly height lines, none wider than width. The port line,
// while it is shown, goes under the header line and takes a line from the body.
func (m *Model) render() string {
	w, h := m.size()
	header := m.headerView(w)
	if pl := m.portLine(w); pl != "" {
		header += "\n" + pl
	}
	footer := m.footerView(w)
	bh := max(h-lipgloss.Height(header)-lipgloss.Height(footer), 0)
	var body string
	switch {
	case m.sudo.open:
		body = sudoView(w, bh)
	case m.help:
		body = m.helpView(w, bh)
	case m.kill.active():
		body = m.killView(w, bh)
	case m.detail && w >= splitWidth:
		dw := detailWidth(w)
		body = lipgloss.JoinHorizontal(lipgloss.Top, fit(m.tableView(w-dw, bh), w-dw, bh), fit(m.detailView(dw, bh), dw, bh))
	case m.detail:
		body = m.detailView(w, bh)
	default:
		body = m.tableView(w, bh)
	}
	parts := []string{header, footer}
	if bh > 0 {
		parts = []string{header, fit(body, w, bh), footer}
	}
	return fit(strings.Join(parts, "\n"), w, h) // a terminal shorter than header and footer cuts the footer
}

// size is the screen's width and height: the terminal's, or 80 by 24 before the first
// WindowSizeMsg.
func (m *Model) size() (int, int) {
	if m.width <= 0 || m.height <= 0 {
		return minWidth, 24
	}
	return m.width, m.height
}

// fit pads or cuts s to exactly h lines, each exactly w cells wide.
func fit(s string, w, h int) string {
	lines := strings.Split(s, "\n")
	if s == "" {
		lines = nil
	}
	out := make([]string, h)
	for i := range out {
		var l string
		if i < len(lines) {
			l = ansi.Truncate(lines[i], w, "")
		}
		out[i] = l + strings.Repeat(" ", max(w-ansi.StringWidth(l), 0))
	}
	return strings.Join(out, "\n")
}

// selected returns the selected row, or false when there is none.
func (m *Model) selected() (model.Row, bool) {
	if m.selIdx < 0 || m.selIdx >= len(m.rows) {
		return model.Row{}, false
	}
	return m.rows[m.selIdx], true
}
