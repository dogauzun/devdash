package tui

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
)

// DEV-34 owns this file: the kill modal over engine plans (p process, t tree, f force, esc
// cancel), the second confirmation outside every project, refusals, and survivors with the
// offer to force-kill them. Kill runs as a tea.Cmd, off the UI goroutine, and its result
// comes back as an action.

// killStage is where the kill modal is; killClosed is the zero value.
type killStage uint8

const (
	killClosed  killStage = iota
	killRefused           // the target itself is refused: the reason, and only esc closes
	killConfirm           // the plan (or why these options are refused): p, t, f, enter/y, esc
	killOutside           // the target is outside every project: second confirmation
	killRunning           // Kill runs off the UI goroutine; every key but ctrl+c is ignored
	killReport            // Kill finished with survivors or errors: f force-kills survivors, esc closes
)

// killState is the kill modal's state; the zero value is closed.
type killState struct {
	stage killStage
	key   model.RowKey // the target row
	name  string       // the target's name, for titles
	opts  engine.KillOptions
	// plan is what is shown and exactly what Kill gets: it changes only on p, t, f and the
	// survivors' force, never with a new snapshot. Valid when refusal is "".
	plan      engine.Plan
	survivors bool              // plan is the force plan for the survivors of the last Kill
	projects  map[string]string // project names by ID, from the snapshot plan was made from
	refusal   string            // why the target, or the current options, cannot be planned
	result    engine.Result     // killReport
	err       error             // killReport: Kill's own error, nothing was signalled
	top       int               // first list line shown when the list scrolls
	page      int               // list lines the last render showed (the pgup/pgdown step)
}

// killDoneMsg is Kill's answer, delivered back to the UI goroutine.
type killDoneMsg struct {
	result engine.Result
	err    error
}

func (msg killDoneMsg) apply(m *Model) tea.Cmd { return m.killDone(msg.result, msg.err) }

// active reports whether the kill modal has the keyboard.
func (k *killState) active() bool { return k.stage != killClosed }

// startKill opens the kill modal for the selected row (x), in process mode with SIGTERM. A
// refused target opens it with the reason; a header or no selection only sets the status.
func (m *Model) startKill() tea.Cmd {
	r, ok := m.selected()
	if !ok || r.Key.Header != model.GroupNone || r.Process == nil && r.Container == nil {
		m.status = "select a process to kill"
		return nil
	}
	name := ""
	switch {
	case r.Process != nil:
		name = r.Process.Name
	case r.Container != nil:
		name = r.Container.Name
	}
	s := m.upd.Snapshot
	p, err := m.o.Plan(s, r.Key, engine.KillOptions{})
	var ref *engine.Refusal
	switch {
	case errors.As(err, &ref):
		m.kill = killState{stage: killRefused, key: r.Key, name: name, refusal: ref.Reason}
	case err != nil:
		m.status = "cannot kill " + name + ": " + err.Error()
	default:
		m.kill = killState{stage: killConfirm, key: r.Key, name: name, plan: p, projects: killProjects(s)}
	}
	return nil
}

// killReplan plans the target again with o from the latest snapshot (p, t and f).
func (m *Model) killReplan(o engine.KillOptions) {
	k := &m.kill
	k.opts = o
	s := m.upd.Snapshot
	p, err := m.o.Plan(s, k.key, o)
	if err != nil {
		k.plan, k.refusal = engine.Plan{}, err.Error()
		if ref := (*engine.Refusal)(nil); errors.As(err, &ref) {
			k.refusal = ref.Reason
		}
		return
	}
	k.plan, k.refusal, k.projects, k.top = p, "", killProjects(s), 0
}

// killProjects maps project IDs to names in s.
func killProjects(s model.Snapshot) map[string]string {
	ns := make(map[string]string, len(s.Projects))
	for _, p := range s.Projects {
		ns[p.ID] = p.Name
	}
	return ns
}

// killKey handles keys while the modal is open. tui.go routes every key but ctrl+c here.
func (m *Model) killKey(key tea.KeyPressMsg) tea.Cmd {
	k := &m.kill
	s := key.String()
	switch s { // scrolling only reads, so it works in every stage
	case "up", "k":
		m.killScroll(-1)
		return nil
	case "down", "j":
		m.killScroll(1)
		return nil
	case "pgup":
		m.killScroll(-max(k.page, 1))
		return nil
	case "pgdown":
		m.killScroll(max(k.page, 1))
		return nil
	}
	if k.stage == killRunning {
		return nil
	}
	if s == "esc" {
		m.kill = killState{}
		return nil
	}
	switch k.stage {
	case killConfirm:
		switch s {
		case "p":
			m.killReplan(engine.KillOptions{Force: k.opts.Force})
		case "t":
			m.killReplan(engine.KillOptions{Tree: true, Force: k.opts.Force})
		case "f":
			m.killReplan(engine.KillOptions{Tree: k.opts.Tree, Force: !k.opts.Force})
		case "enter", "y":
			switch {
			case k.refusal != "":
			case k.plan.Outside:
				k.stage = killOutside
			default:
				return m.killSignal(k.plan)
			}
		}
	case killOutside:
		if s == "enter" || s == "y" {
			return m.killSignal(k.plan)
		}
	case killReport:
		if sv := k.result.Survivors(); s == "f" && k.err == nil && len(sv) > 0 {
			// Every survivor passed NewPlan's refusals when the target was planned, and Kill
			// re-checks each pid (never 0, 1 or devdash's chain) and its start time.
			k.survivors = true
			return m.killSignal(engine.Plan{Procs: sv, Signal: syscall.SIGKILL})
		}
	}
	return nil
}

// killSignal shows p as being signalled and returns the command that runs Kill on it.
func (m *Model) killSignal(p engine.Plan) tea.Cmd {
	m.kill.stage, m.kill.plan, m.kill.top = killRunning, p, 0
	kill, timeout := m.o.Kill, m.o.KillTimeout
	return func() tea.Msg {
		r, err := kill(p, timeout)
		return killDoneMsg{r, err}
	}
}

// killDone takes Kill's answer: it asks for a refresh when anything may have been signalled,
// then closes the modal with a summary when every planned process is gone, or reports.
func (m *Model) killDone(r engine.Result, err error) tea.Cmd {
	if m.kill.stage != killRunning {
		return nil
	}
	if err == nil {
		m.o.Source.Refresh()
	}
	all := err == nil && len(r.Outcomes) > 0
	killed, gone := 0, 0
	for _, o := range r.Outcomes {
		all = all && o.Exited
		switch {
		case o.Signalled:
			killed++
		case o.Exited:
			gone++
		}
	}
	if !all {
		m.kill.stage, m.kill.result, m.kill.err, m.kill.top = killReport, r, err, 0
		return nil
	}
	m.status = "killed " + killCount(killed, "process")
	if gone > 0 {
		m.status += fmt.Sprintf(", %d already gone", gone)
	}
	m.kill = killState{}
	return nil
}

// killView draws the modal in w by h: a title line, the processes indented below it, then
// notes and the key hints. No borders (spec: no box drawing beyond table borders); a list
// longer than the room scrolls. Lines wider than w are cut by render.
func (m *Model) killView(w, h int) string {
	k := &m.kill
	title := "kill " + k.name
	if k.key.PID > 0 {
		title += fmt.Sprintf(" (pid %d)", k.key.PID)
	}
	var list, tail []string
	switch k.stage {
	case killRefused:
		title = "cannot kill " + k.name
		tail = append(killReason(k.refusal), "", "esc close")
	case killConfirm, killOutside, killRunning:
		switch {
		case k.refusal != "":
			title += ": " + killMode(k.opts)
			tail = killReason(k.refusal)
			tail[0] = "refused: " + tail[0]
			tail = append(tail, "", "p process  t tree  f force  esc cancel")
			return m.killLayout(w, h, styleBold.Render(title), nil, tail)
		case k.survivors:
			title += ": force-kill survivors, SIGKILL to " + killCount(len(k.plan.Procs), "process")
		default:
			title += fmt.Sprintf(": %s, %s to %s", killMode(k.opts), killSig(k.plan.Signal), killCount(len(k.plan.Procs), "process"))
		}
		list = m.killPlanLines(k.plan)
		if k.plan.Group != 0 {
			tail = append(tail, fmt.Sprintf("pid %d leads its process group: the signal goes to the whole group", k.plan.Group))
		}
		switch k.stage {
		case killConfirm:
			tail = append(tail, "p process  t tree  f force  enter confirm  esc cancel")
		case killOutside:
			tail = append(tail, styleWarn.Render(k.name+" is outside every project, usually a system service."),
				"Confirm again: enter or y   esc cancel")
		case killRunning:
			tail = append(tail, fmt.Sprintf("signalling %s, waiting up to %s", killSig(k.plan.Signal), m.o.KillTimeout))
		}
	case killReport:
		if k.err != nil {
			title += ": nothing was signalled"
			tail = []string{k.err.Error(), "", "esc close"}
			break
		}
		exited := 0
		var rows []string
		for _, o := range k.result.Outcomes {
			if o.Exited {
				exited++
				continue
			}
			rows = append(rows, fmt.Sprintf("%d\t%s\t%s", o.Process.PID, o.Process.Name, killOutcome(o)))
		}
		title += fmt.Sprintf(": %d of %s exited after %s", exited, killCount(len(k.result.Outcomes), "process"), killSig(k.plan.Signal))
		list = killTable(rows)
		tail = []string{"esc close"}
		if len(k.result.Survivors()) > 0 {
			tail = []string{"f force-kill survivors (SIGKILL)  esc close"}
		}
	}
	return m.killLayout(w, h, styleBold.Render(title), list, tail)
}

// killReason splits a refusal before its hint (the engine puts it after the last ": ", as in
// "...: use docker stop shop-db-1"), so the command to copy is on a line of its own.
func killReason(r string) []string {
	if i := strings.LastIndex(r, ": "); i >= 0 {
		return []string{r[:i+1], r[i+2:]}
	}
	return []string{r}
}

// killPlanLines is one line per planned process, in signal order: pid, name, project and ports.
func (m *Model) killPlanLines(p engine.Plan) []string {
	rows := make([]string, 0, len(p.Procs))
	for _, proc := range p.Procs {
		project := m.kill.projects[proc.ProjectID]
		if project == "" {
			project = "-"
		}
		rows = append(rows, fmt.Sprintf("%d\t%s\t%s\t%s", proc.PID, proc.Name, project, killPorts(proc)))
	}
	return killTable(rows)
}

// killTable aligns tab-separated rows into columns two spaces apart.
func killTable(rows []string) []string {
	if len(rows) == 0 {
		return nil
	}
	var b bytes.Buffer
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		fmt.Fprintln(tw, r)
	}
	_ = tw.Flush()
	return strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
}

// killLayout stacks the title, a blank line, the list, a blank line and the tail, list and
// tail indented, in w by h: tail lines (reasons, prompts, key hints) are wrapped to the width
// so none is cut. A list longer than the room left scrolls: it shows the page from
// m.kill.top (clamped here, and the page size kept for the keys) and a position line; the
// blank lines go first when not even one list line fits.
func (m *Model) killLayout(w, h int, title string, list, tail []string) string {
	k := &m.kill
	var wrapped []string
	for _, l := range tail {
		wrapped = append(wrapped, strings.Split(ansi.Wrap(l, max(w-2, 1), ""), "\n")...)
	}
	tail = wrapped
	blank := []string{""}
	gaps := 2
	if len(list) == 0 {
		gaps = 1
	}
	room := h - 1 - len(tail) - gaps
	if room < min(len(list), 1) {
		blank, room = nil, h-1-len(tail)
	}
	k.page = len(list)
	if len(list) > room {
		n := max(room-1, 0) // pid lines; the last line of the room is the position
		k.page = max(n, 1)
		k.top = max(min(k.top, len(list)-n), 0)
		pos := fmt.Sprintf("%d pids", len(list))
		if n > 0 {
			pos = fmt.Sprintf("pids %d-%d of %d, ↑↓ to scroll", k.top+1, k.top+n, len(list))
		}
		list = append(list[k.top:k.top+n:k.top+n], pos)
		if room <= 0 {
			list = nil
		}
	} else {
		k.top = 0
	}
	out := []string{title}
	out = append(out, blank...)
	for _, l := range list {
		out = append(out, "  "+l)
	}
	if len(list) > 0 {
		out = append(out, blank...)
	}
	for _, l := range tail {
		out = append(out, "  "+l)
	}
	return strings.Join(out, "\n")
}

// killScroll moves the list by d lines, within the list as the last render laid it out.
func (m *Model) killScroll(d int) {
	k := &m.kill
	n := len(k.plan.Procs)
	if k.stage == killReport {
		n = 0
		for _, o := range k.result.Outcomes {
			if !o.Exited {
				n++
			}
		}
	}
	k.top = max(min(k.top+d, n-max(k.page, 1)), 0)
}

// killMode names the options as the CLI does: "process mode", "tree mode, force".
func killMode(o engine.KillOptions) string {
	mode := "process mode"
	if o.Tree {
		mode = "tree mode"
	}
	if o.Force {
		mode += ", force"
	}
	return mode
}

// killSig names a plan's signal.
func killSig(s syscall.Signal) string {
	if s == syscall.SIGKILL {
		return "SIGKILL"
	}
	return "SIGTERM"
}

// killCount is "1 process", "3 processes".
func killCount(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ses", n, what)
}

// killPorts lists p's listening ports, sorted and without repeats, or "-".
func killPorts(p model.Process) string {
	var ns []int
	for _, l := range p.Listeners {
		ns = append(ns, int(l.Port))
	}
	slices.Sort(ns)
	ns = slices.Compact(ns)
	if len(ns) == 0 {
		return "-"
	}
	ss := make([]string, len(ns))
	for i, n := range ns {
		ss[i] = strconv.Itoa(n)
	}
	return strings.Join(ss, ",")
}

// killOutcome says why a planned process is still there after Kill.
func killOutcome(o engine.Outcome) string {
	switch {
	case errors.Is(o.Err, engine.ErrPermission):
		return "permission denied, run with sudo"
	case errors.Is(o.Err, engine.ErrStartTime):
		return "not signalled: pid reused"
	case o.Err != nil:
		return "not signalled: " + o.Err.Error()
	case o.Signalled:
		return "still running"
	}
	return "not signalled"
}
