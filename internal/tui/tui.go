// Package tui is the dashboard: a Bubble Tea v2 program that renders the engine's snapshots as
// one screen (header, tree table grouped by project, footer) with a detail pane, a filter and
// the kill modal on demand. It is a thin client of the engine: everything it shows comes from
// the latest engine.Update, and actions go through the engine's plan and kill functions.
//
// One file per feature: tui.go (state, message routing, layout), header.go (header and footer),
// table.go (rows, columns, movement and view toggles), rows.go (flattening, selection that
// survives a refresh, the filter), detail.go, help.go, open.go and kill.go. Each feature
// file defines its own state type, held in Model, and its own messages, which implement action.
package tui

import (
	"context"
	"errors"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/dogauzun/devdash/internal/engine"
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
	// DockerSocket returns the Docker endpoint in use, shown in the detail pane of a container
	// row; nil or "" shows nothing.
	DockerSocket func() string
	Now          func() time.Time // time.Now when nil; tests fix it
	ShowAll      bool             // --all: start with shells and editors shown
}

// Model is the dashboard's state. Use New; the zero value is not ready.
type Model struct {
	o Options

	width, height int

	upd  engine.Update // latest update from the engine
	have bool          // upd holds a good snapshot (SchemaVersion != 0)

	view model.ViewOptions // ShowAll, HideContainers, Sort, Collapsed (table.go)
	all  []model.Row       // m.upd.Snapshot flattened with m.view
	rows []model.Row       // all, filtered: what the table shows

	sel    model.RowKey // selected row; never an index (rows.go)
	selIdx int          // index of sel in rows, -1 when rows is empty
	top    int          // first table row on screen (table.go)
	tcache tableCache   // derived from all rows, per rebuild (table.go)

	filter    string // active filter query (rows.go)
	filtering bool   // the filter prompt has the keyboard

	fsel filterSel // the row chosen before the filter hid it (rows.go)

	detail bool // detail pane open (detail.go)
	help   bool // help overlay open (help.go)
	kill   killState
	hpos   helpPos // help overlay scroll position (help.go)

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
	if o.Now == nil {
		o.Now = time.Now
	}
	m := &Model{o: o, selIdx: -1, view: model.ViewOptions{ShowAll: o.ShowAll, Collapsed: map[model.RowKey]bool{}}}
	m.rebuild()
	return m
}

// Run starts the dashboard on the terminal and blocks until the user quits, ctx is done or
// the engine stops.
func Run(ctx context.Context, o Options, opts ...tea.ProgramOption) error {
	opts = append([]tea.ProgramOption{tea.WithContext(ctx)}, opts...)
	_, err := tea.NewProgram(New(o), opts...).Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil { // bubbletea wraps ctx.Err() into it
		return nil
	}
	return err
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

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case updateMsg:
		m.upd = engine.Update(msg)
		m.have = m.upd.Snapshot.SchemaVersion != 0
		m.rebuild()
		return m, m.wait()
	case closedMsg:
		return m, tea.Quit
	case tickMsg:
		return m, tick()
	case tea.KeyPressMsg:
		m.status = ""
		return m, m.key(msg)
	case tea.PasteMsg:
		m.paste(msg.Content)
	case action:
		return m, msg.apply(m)
	}
	return m, nil
}

// key routes a key press: ctrl+c always quits; an open kill modal, help overlay or filter
// prompt takes every other key; otherwise the global keys, then the table's.
func (m *Model) key(k tea.KeyPressMsg) tea.Cmd {
	s := k.String()
	if s == "ctrl+c" {
		return tea.Quit
	}
	switch {
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
		m.detail = !m.detail
	case "esc":
		switch {
		case m.detail:
			m.detail = false
		case m.filter != "":
			m.setFilter("")
		}
	case "x":
		return m.startKill()
	case "o":
		return m.openSelected()
	default:
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

// render draws the whole screen: exactly height lines, none wider than width.
func (m *Model) render() string {
	w, h := m.width, m.height
	if w <= 0 || h <= 0 {
		w, h = minWidth, 24 // before the first WindowSizeMsg
	}
	header := m.headerView(w)
	footer := m.footerView(w)
	bh := max(h-lipgloss.Height(header)-lipgloss.Height(footer), 0)
	var body string
	switch {
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
