package tui

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
)

// DEV-34 owns this file: the kill modal over engine plans (p process, t tree, f force, esc
// cancel), the second confirmation outside every project (Y only), refusals, and survivors
// with the offer to force-kill them. Kill runs as a tea.Cmd, off the UI goroutine, and its result
// comes back as an action. DEV-136: the summary adds whether the killed processes' ports are
// free, from the first snapshot taken after the kill.

// killStage is where the kill modal is; killClosed is the zero value.
type killStage uint8

const (
	killClosed  killStage = iota
	killRefused           // the target itself is refused: the reason, and only esc closes
	killConfirm           // the plan (or why these options are refused): p, t, f, enter/y, esc
	killOutside           // the target is outside every project: second confirmation, Y only
	killRunning           // Kill runs off the UI goroutine; every key but ctrl+c is ignored
	killReport            // Kill finished with survivors or errors: f force-kills survivors, esc closes
)

// killTarget is a process the modal plans from: its row key and its name, for titles.
type killTarget struct {
	key  model.RowKey
	name string
}

// killState is the kill modal's state; the zero value is closed.
type killState struct {
	stage      killStage
	killTarget // what the plan is from and the titles name: own, or tree in tree mode
	// own is the selected row's process; tree is where tree mode plans from: on a folded row
	// the chain's first process as its label draws it (DEV-179), otherwise own.
	own, tree killTarget
	opts      engine.KillOptions
	// plan is what is shown and exactly what Kill gets: it changes only on p, t, f and the
	// survivors' force, never with a new snapshot. Valid when refusal is "".
	plan      engine.Plan
	survivors bool              // plan is the force plan for the survivors of the last Kill
	projects  map[string]string // project names by ID, from the snapshot plan was made from
	refusal   string            // why the target, or the current options, cannot be planned
	result    engine.Result     // killReport
	earlier   []engine.Outcome  // the earlier rounds' outcomes but the survivors force-killed since
	err       error             // killReport: Kill's own error, nothing was signalled
	top       int               // first list line shown when the list scrolls
	page      int               // list lines the last render showed (the pgup/pgdown step)
	// blind is set by the last render when not one pid line fitted: confirm is then neither
	// offered nor accepted. Bubble Tea renders after every message, so it is what the user saw.
	blind bool
}

// killDoneMsg is Kill's answer, delivered back to the UI goroutine, with when Kill returned.
type killDoneMsg struct {
	result engine.Result
	err    error
	done   time.Time
}

func (msg killDoneMsg) apply(m *Model) tea.Cmd { return m.killDone(msg.result, msg.err, msg.done) }

// killAfter is a finished kill's ports, waiting for the first snapshot taken after it to say
// whether each is free (spec "Release 1.1", Kill result); the zero value waits for nothing.
type killAfter struct {
	ports []uint16  // the signalled processes' TCP ports, ascending, without repeats
	done  time.Time // when Kill returned: a snapshot taken before may still list them
}

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
	case r.Process != nil && (r.Process.PID != 0 || r.Container == nil):
		name = r.Process.Label()
	case r.Container != nil: // a container row, or its port's unreadable PID 0 owner
		name = r.Container.Name
	}
	own := killTarget{r.Key, name}
	tree := own
	if ls := m.drawnLinks(r.Links); len(ls) > 0 { // the label's rule, so the plan and the label agree
		tree = killTarget{ls[0].Key(), ls[0].Label()}
	}
	s := m.upd.Snapshot
	p, err := m.o.Plan(s, r.Key, engine.KillOptions{})
	var ref *engine.Refusal
	switch {
	case errors.As(err, &ref):
		m.kill = killState{stage: killRefused, killTarget: own, refusal: ref.Reason}
	case err != nil:
		m.status = "cannot kill " + name + ": " + err.Error()
	default:
		m.kill = killState{stage: killConfirm, killTarget: own, own: own, tree: tree, plan: p, projects: killProjects(s)}
	}
	return nil
}

// killReplan plans again with o from the latest snapshot (p, t and f): from k.tree in tree
// mode, from k.own otherwise.
func (m *Model) killReplan(o engine.KillOptions) {
	k := &m.kill
	k.opts, k.killTarget = o, k.own
	if o.Tree {
		k.killTarget = k.tree
	}
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
	// A held key repeats: it must never confirm, so repeats do nothing here. Most terminals do
	// not mark repeats (Bubble Tea sets IsRepeat only with Kitty keyboard enhancements), hence
	// also the second confirmation's key, which the first prompt does not take.
	if k.stage == killRunning || key.IsRepeat {
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
			case k.refusal != "" || k.blind:
			case k.plan.Outside:
				k.stage = killOutside
			default:
				return m.killSignal(k.plan)
			}
		}
	case killOutside:
		if s == "Y" && !k.blind {
			return m.killSignal(k.plan)
		}
	case killReport:
		if sv := k.result.Survivors(); s == "f" && k.err == nil && len(sv) > 0 {
			// Every survivor passed NewPlan's refusals when the target was planned, and Kill
			// re-checks each pid (never 0, 1 or devdash's chain) and its start time.
			// k.result already holds the rounds before it (killDone), so earlier is replaced.
			k.survivors, k.earlier = true, nil
			for _, o := range k.result.Outcomes {
				if !o.Signalled || o.Exited {
					k.earlier = append(k.earlier, o)
				}
			}
			return m.killSignal(engine.Plan{Procs: sv, Signal: syscall.SIGKILL})
		}
	}
	return nil
}

// killSignal shows p as being signalled and returns the command that runs Kill on it.
func (m *Model) killSignal(p engine.Plan) tea.Cmd {
	m.kill.stage, m.kill.plan, m.kill.top = killRunning, p, 0
	kill, timeout, clock := m.o.Kill, m.o.KillTimeout, m.o.Now
	return func() tea.Msg {
		r, err := kill(p, timeout)
		return killDoneMsg{r, err, clock()}
	}
}

// killDone takes Kill's answer: it asks for a refresh when anything may have been signalled,
// then closes the modal with a summary when every planned process is gone, or reports. Both
// cover the whole kill, earlier rounds included, so a process an earlier round did not signal
// keeps the report open (DEV-165); the summary waits for the first snapshot taken after done to
// add the processes' ports.
func (m *Model) killDone(r engine.Result, err error, done time.Time) tea.Cmd {
	if m.kill.stage != killRunning {
		return nil
	}
	if err == nil {
		m.o.Source.Refresh()
	}
	all := err == nil && len(r.Outcomes) > 0
	killed, gone := 0, 0
	outcomes := slices.Concat(m.kill.earlier, r.Outcomes)
	for _, o := range outcomes {
		m.sudo.denied = m.sudo.denied || errors.Is(o.Err, engine.ErrPermission) // sudo.go
		all = all && o.Exited
		switch {
		case o.Signalled:
			killed++
		case o.Exited:
			gone++
		}
	}
	if !all {
		r.Outcomes = outcomes
		m.kill.stage, m.kill.result, m.kill.err, m.kill.top = killReport, r, err, 0
		return nil
	}
	m.status = "killed " + killCount(killed, "process")
	if gone > 0 {
		m.status += fmt.Sprintf(", %d already gone", gone)
	}
	m.kill = killState{}
	var ports []uint16
	for _, o := range outcomes {
		if !o.Signalled { // gone before the signal: not one of the processes this kill stopped
			continue
		}
		for _, l := range o.Process.Listeners {
			if !strings.HasPrefix(l.Proto, "udp") {
				ports = append(ports, l.Port)
			}
		}
	}
	slices.Sort(ports)
	m.kafter = killAfter{ports: slices.Compact(ports), done: done}
	return nil
}

// killPorts adds the last kill's ports to its summary in the status line once a snapshot taken
// after the kill finished is in: `5173 free`, or `5173 still held by` its holders, each
// process by its label and pid, a container by its name, comma-joined. Free means
// model.Holders finds none, as `kill N` checks: no listener and no published container port,
// from the snapshot alone. Update calls it on each new snapshot; a key clears the wait.
func (m *Model) killPorts() {
	k := m.kafter
	if len(k.ports) == 0 || !m.have || !m.upd.Snapshot.TakenAt.After(k.done) {
		return
	}
	m.kafter = killAfter{}
	s := m.upd.Snapshot
	for _, port := range k.ports {
		hs := model.Holders(s, port)
		if len(hs) == 0 {
			m.status += fmt.Sprintf("%s%d free", headerSep, port)
			continue
		}
		who := make([]string, len(hs))
		for i, h := range hs {
			who[i] = killHolder(s, h)
		}
		m.status += fmt.Sprintf("%s%d still held by %s", headerSep, port, strings.Join(who, ", "))
	}
}

// killHolder names a port's holder after a kill: a process by its label and pid, the unknown
// owner as such, a container by its name (its ID when it has none). The status is cleaned when
// drawn.
func killHolder(s model.Snapshot, h model.Holder) string {
	p := h.Process
	switch {
	case p == nil:
		for _, c := range s.Containers {
			if c.ID == h.Key.ContainerID && c.Name != "" {
				return c.Name
			}
		}
		return h.Key.ContainerID
	case p.PID == 0:
		return "unknown owner"
	}
	return fmt.Sprintf("%s %d", p.Label(), p.PID)
}

// killView draws the modal in w by h: a title line, the processes indented below it, then
// notes and the key hints. No borders (spec: no box drawing beyond table borders); a list
// longer than the room scrolls. Lines wider than w are cut by render. Names, refusals and
// errors are snapshot text, cleaned here.
func (m *Model) killView(w, h int) string {
	k := &m.kill
	name := model.Clean(k.name)
	head, rest := "kill ", "" // the title is head, name, rest: killTitle cuts name
	if k.key.PID > 0 {
		rest = fmt.Sprintf(" (pid %d)", k.key.PID)
	}
	var list, tail []string
	switch k.stage {
	case killRefused:
		head, rest = "cannot kill ", ""
		tail = append(killReason(model.Clean(k.refusal)), "", "esc close")
	case killConfirm, killOutside, killRunning:
		switch {
		case k.refusal != "":
			rest += ": " + killMode(k.opts)
			tail = killReason(model.Clean(k.refusal))
			tail[0] = "refused: " + tail[0]
			tail = append(tail, "", "p process  t tree  f force  esc cancel")
			return m.killLayout(w, h, killTitle(head, name, rest, w), nil, tail)
		case k.survivors:
			rest += ": force-kill survivors, SIGKILL to " + killCount(len(k.plan.Procs), "process")
		default:
			rest += fmt.Sprintf(": %s, %s to %s", killMode(k.opts), killSig(k.plan.Signal), killCount(len(k.plan.Procs), "process"))
		}
		list = m.killPlanLines(k.plan)
		if k.plan.Group != 0 {
			tail = append(tail, fmt.Sprintf("pid %d leads its process group: the signal goes to the whole group", k.plan.Group))
		}
		if k.stage == killOutside {
			tail = append(tail, styleWarn.Render(name+" is outside every project, usually a system service."))
		}
		hint := ""
		switch k.stage {
		case killConfirm:
			hint = "p process  t tree  f force  enter confirm  esc cancel"
		case killOutside:
			hint = "Confirm again: press Y (shift+y)  esc cancel"
		case killRunning:
			hint = fmt.Sprintf("signalling %s, waiting up to %s", killSig(k.plan.Signal), m.o.KillTimeout)
		}
		// Confirm only what can be seen: when not one pid line fits beside the hints, confirm
		// is off until the terminal grows.
		k.blind = k.stage != killRunning && h-1-len(killWrap(append(tail, hint), w)) < 1
		if k.blind {
			hint = "too small to show the plan: enlarge to confirm  esc cancel"
		}
		tail = append(tail, hint)
	case killReport:
		if k.err != nil {
			rest += ": nothing was signalled"
			tail = []string{model.Clean(k.err.Error()), "", "esc close"}
			break
		}
		exited := 0
		var rows []string
		for _, o := range k.result.Outcomes {
			if o.Exited {
				exited++
				continue
			}
			outcome := model.Clean(killOutcome(o))
			if errors.Is(o.Err, engine.ErrPermission) {
				outcome = styleWarn.Render(outcome) // the last column: tabwriter does not pad it
			}
			rows = append(rows, fmt.Sprintf("%d\t%s\t%s", o.Process.PID, model.Clean(o.Process.Label()), outcome))
		}
		rest += fmt.Sprintf(": %d of %s exited after %s", exited, killCount(len(k.result.Outcomes), "process"), killSig(k.plan.Signal))
		list = killTable(rows)
		tail = []string{"esc close"}
		if len(k.result.Survivors()) > 0 {
			tail = []string{"f force-kill survivors (SIGKILL)  esc close"}
		}
		if m.o.Sudo && slices.ContainsFunc(k.result.Outcomes, func(o engine.Outcome) bool { return errors.Is(o.Err, engine.ErrPermission) }) {
			tail[0] += ", then " + sudoHint // S works in the table, not in this modal (sudo.go)
		}
	}
	return m.killLayout(w, h, killTitle(head, name, rest, w), list, tail)
}

// killTitleMin is the fewest cells a cut label keeps in a kill title.
const killTitleMin = 6

// killTitle is the modal's one-line title, bold: head, name and rest. When it is wider than w,
// name is cut with `…` (as pad cuts a table cell, by display width) so that rest, the pid, mode,
// signal and count, shows whole; name keeps at least killTitleMin cells, and a line still too
// wide is cut at the edge by render (DEV-184).
func killTitle(head, name, rest string, w int) string {
	room := max(w-ansi.StringWidth(head+rest), killTitleMin)
	return styleBold.Render(head + ansi.Truncate(name, room, "…") + rest)
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
		rows = append(rows, fmt.Sprintf("%d\t%s\t%s\t%s", proc.PID, model.Clean(proc.Label()), model.Clean(project), killPorts(proc)))
	}
	return killTable(rows)
}

// killTable aligns tab-separated rows into columns two spaces apart, padded by display width
// as the table's are; the last column is not padded.
func killTable(rows []string) []string {
	if len(rows) == 0 {
		return nil
	}
	cells := make([][]string, len(rows))
	var widths []int
	for i, r := range rows {
		cells[i] = strings.Split(r, "\t")
		for j, c := range cells[i][:len(cells[i])-1] {
			if j == len(widths) {
				widths = append(widths, 0)
			}
			widths[j] = max(widths[j], ansi.StringWidth(c))
		}
	}
	lines := make([]string, len(rows))
	for i, cs := range cells {
		for j, c := range cs[:len(cs)-1] {
			lines[i] += pad(c, widths[j]+2, false)
		}
		lines[i] += cs[len(cs)-1]
	}
	return lines
}

// killWrap wraps tail lines (reasons, prompts, key hints) to the modal's width, indent
// included, so none is cut.
func killWrap(tail []string, w int) []string {
	var wrapped []string
	for _, l := range tail {
		wrapped = append(wrapped, strings.Split(ansi.Wrap(l, max(w-2, 1), ""), "\n")...)
	}
	return wrapped
}

// killLayout stacks the title, a blank line, the list, a blank line and the tail, list and
// tail indented, in w by h, with the tail wrapped by killWrap. A pid line is worth more than
// a blank line: when the list does not fit whole, the blank lines go first. A list still
// longer than the room scrolls: it shows the page from m.kill.top (clamped here, and the
// page size kept for the keys) and a position line, or a single pid line when only one line
// is left (the title has the count).
func (m *Model) killLayout(w, h int, title string, list, tail []string) string {
	k := &m.kill
	tail = killWrap(tail, w)
	blank := []string{""}
	gaps := 2
	if len(list) == 0 {
		gaps = 1
	}
	room := h - 1 - len(tail) - gaps
	if room < len(list) {
		blank, room = nil, h-1-len(tail)
	}
	k.page = len(list)
	if len(list) > room {
		n := max(room-1, 0) // pid lines; the last line of the room is the position,
		if room == 1 {
			n = 1 // unless only one line is left: a pid line beats a position line
		}
		k.page = max(n, 1)
		k.top = max(min(k.top, len(list)-n), 0)
		page := list[k.top : k.top+n : k.top+n]
		if n < room {
			page = append(page, fmt.Sprintf("pids %d-%d of %d, ↑↓ to scroll", k.top+1, k.top+n, len(list)))
		}
		list = page
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
