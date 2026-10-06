package model

import (
	"cmp"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// GroupKind says what a header row groups; the zero value marks a row that is not a header.
type GroupKind uint8

// Group kinds.
const (
	GroupNone       GroupKind = iota // not a header
	GroupProject                     // a git repository; RowKey.Group is Project.ID
	GroupCompose                     // a compose project; RowKey.Group is its name
	GroupContainers                  // containers without a compose label
	GroupOther                       // processes outside any project, sorted last
)

// RowKey identifies a row across snapshots; the TUI stores selection and expansion by it,
// never by index. Exactly one kind of row sets each part, so no two rows share a key and no
// row has the zero key:
//   - process row: (PID, StartTime); the PID 0 "unknown" rows, which share both, add their one Listener;
//   - container row with no process behind it: ContainerID;
//   - header row: Header, plus Group for project and compose headers.
type RowKey struct {
	PID         int
	StartTime   int64     // StartTime.UnixNano()
	Listener    Listener  // PID 0 rows only
	ContainerID string    // container rows with no process only
	Header      GroupKind // header rows only
	Group       string    // Project.ID or compose project name; "" for GroupContainers and GroupOther
}

// Key returns the row key of p.
func (p Process) Key() RowKey {
	k := RowKey{PID: p.PID, StartTime: p.StartTime.UnixNano()}
	if p.PID == 0 && len(p.Listeners) > 0 {
		k.Listener = p.Listeners[0]
		k.Listener.ContainerID = "" // set only while Docker answers; the row is the same either way
	}
	return k
}

// Row is one line of the table both the TUI and the CLI render: a header, a process, or a
// container with no process behind it. Pointers point into the Snapshot given to Flatten.
type Row struct {
	Key       RowKey
	Project   *Project   // GroupProject headers
	Process   *Process   // process rows
	Container *Container // container rows, and process rows with a ContainerID (for name and image)
	// Depth is 0 for headers and grows by one per tree level below them. Rows are in preorder,
	// so a row's ancestors are the nearest preceding rows with a smaller Depth; a filter over
	// []Row keeps ancestors of a match by walking back.
	Depth  int
	Dimmed bool // hidden by the view (shell, editor; the TUI's search also container rows with `d`) but drawn to keep a visible descendant connected
	// Links are the processes of a folded chain before this row's own, first to last
	// (ViewOptions.Fold): the row is the chain's last process and sits at its first one's depth.
	Links []*Process
}

// ChainSep joins the labels of a folded chain's processes (DEV-157).
const ChainSep = " › "

// SortMode orders the roots within each group (the `s` key cycles through them).
type SortMode uint8

// Sort modes.
const (
	SortDefault SortMode = iota // listeners first, then start time
	SortPort                    // lowest port first
	SortCPU                     // highest CPU percent first
	SortStart                   // most recently started first
	SortName                    // by the name column's label, case-insensitive
)

// ViewOptions are the view toggles Flatten honours. The text filter is not one: it runs on
// the returned rows (see Row.Depth).
type ViewOptions struct {
	ShowAll        bool            // `a` / --all: show shells and editors
	Search         bool            // the TUI's search: show shells and editors as ShowAll does, which still names the folded labels (DEV-160)
	HideContainers bool            // `d`: drop container rows (processes with a ContainerID and containers without one)
	Sort           SortMode        // `s`
	Collapsed      map[RowKey]bool // collapsed headers and tree nodes
	Fold           bool            // fold chains of single-child processes into one row (DEV-157)
}

// Flatten turns a snapshot into display order (spec "Process tree and kinds" and "TUI design"):
// one header per group, the Here project first (spec "Release 1.0", TUI), then ordered by most
// recent activity with `containers` and `other` last,
// each followed by its process tree by ppid with roots sorted by opts.Sort; containers with no
// process become rows under their compose project or `containers`; hidden kinds are dropped
// unless ShowAll or Search or needed to connect a visible descendant (then Dimmed); children of collapsed
// keys are skipped. s must not be modified while the rows are in use.
//
// With Fold, a chain is drawn as one row (DEV-157): a maximal run of processes P1 → … → Pn
// (n ≥ 2) in which every process before the last has exactly one shown child, the next, and
// no listener, tag, ContainerID or collapsed key, so that no port or tag leaves the table. The
// row is Pn's (key, fields, Dimmed, children one level below it) with P1…Pn-1 as its Links, at
// P1's depth, and sorts among its siblings by Pn, or in name mode by the chain's label. The TUI
// unfolds a chain itself, from these rows (DEV-193).
//
// A process whose argv is known to be empty (not merely unreadable) and that has no listener
// is never a row. A group with no row to show
// is omitted. Equal sort keys fall back to the row key, so the order never depends on the
// order of s.Processes.
func Flatten(s Snapshot, opts ViewOptions) []Row {
	ordered := slices.SortedFunc(maps.Values(groupsOf(s, opts)), func(a, b *group) int {
		ka, kb := a.header.Key, b.header.Key
		return cmp.Or(
			boolCompare(!a.here(), !b.here()),
			cmp.Compare(lastRank(ka.Header), lastRank(kb.Header)),
			b.active.Compare(a.active),
			cmp.Compare(ka.Header, kb.Header),
			cmp.Compare(ka.Group, kb.Group))
	})
	var rows []Row
	for _, g := range ordered {
		roots := g.tree()
		shown := false
		for _, n := range roots {
			shown = n.mark() || shown
		}
		if !shown {
			continue
		}
		order(roots, &opts)
		rows = append(rows, g.header)
		if opts.Collapsed[g.header.Key] {
			continue
		}
		for _, n := range roots {
			rows = n.emit(rows, 1, &opts)
		}
	}
	return rows
}

// groupsOf returns the groups of s by header key, each with its nodes in s's order and its
// latest start time: a node per process (skipping one with a known empty argv and no listener)
// and per container with a published port and no process behind it, both dropped with
// opts.HideContainers. Nodes carry their sort keys (the name in name mode only) and hidden.
func groupsOf(s Snapshot, opts ViewOptions) map[RowKey]*group {
	projects := make(map[string]*Project, len(s.Projects))
	for i := range s.Projects {
		projects[s.Projects[i].ID] = &s.Projects[i]
	}
	containers := make(map[string]*Container, len(s.Containers))
	for i := range s.Containers {
		containers[s.Containers[i].ID] = &s.Containers[i]
	}

	groups := map[RowKey]*group{}
	add := func(header RowKey, n *node) {
		g := groups[header]
		if g == nil {
			g = &group{header: Row{Key: header}}
			if header.Header == GroupProject {
				g.header.Project = projects[header.Group]
			}
			groups[header] = g
		}
		g.nodes = append(g.nodes, n)
		if opts.Sort == SortName {
			n.name = strings.ToLower(n.sortName())
		}
		if n.start.After(g.active) {
			g.active = n.start
		}
	}
	withProcess := map[string]bool{}
	for i := range s.Processes {
		p := &s.Processes[i]
		if p.PID != 0 && len(p.Argv) == 0 && p.Unknown&FieldArgv == 0 && len(p.Listeners) == 0 {
			continue
		}
		n := &node{row: Row{Key: p.Key(), Process: p}, start: p.StartTime, cpu: p.CPUPercent, port: noPort,
			hidden: !opts.ShowAll && !opts.Search && p.Hideable()}
		for _, l := range p.Listeners {
			n.port = min(n.port, int(l.Port))
		}
		header := RowKey{Header: GroupOther}
		switch {
		case p.ContainerID != "":
			withProcess[p.ContainerID] = true
			if opts.HideContainers {
				continue
			}
			n.row.Container = containers[p.ContainerID]
			header = containerGroup(n.row.Container)
		case projects[p.ProjectID] != nil:
			header = RowKey{Header: GroupProject, Group: p.ProjectID}
		}
		add(header, n)
	}
	for i := range s.Containers {
		c := &s.Containers[i]
		if opts.HideContainers || withProcess[c.ID] {
			continue
		}
		n := &node{row: Row{Key: RowKey{ContainerID: c.ID}, Container: c}, cpu: math.NaN(), port: noPort}
		for _, m := range c.Ports {
			if m.HostPort != 0 {
				n.port = min(n.port, int(m.HostPort))
			}
		}
		if n.port != noPort { // only a published port makes a container without a process a row
			add(containerGroup(c), n)
		}
	}
	return groups
}

// Hideable reports whether Flatten hides p unless ShowAll: a shell or an editor with no
// listener (a listener is never hidden).
func (p *Process) Hideable() bool {
	return len(p.Listeners) == 0 && (p.Kind == KindShell || p.Kind == KindEditor)
}

// group is one header and the rows under it, before the tree is built.
type group struct {
	header Row
	nodes  []*node
	active time.Time // latest start time of its processes, hidden ones included
}

// node is one non-header row with its sort keys and tree links.
type node struct {
	row      Row
	start    time.Time
	cpu      float64
	port     int    // lowest listener or published port, noPort when none
	name     string // sortName lower-cased, set for SortName only
	hidden   bool
	show     bool // not hidden, or has a descendant that is not hidden
	children []*node
	links    []*Process // with Fold: the chain's processes before last (fold)
	last     *node      // the last node of the chain n starts; nil when it starts none
}

const noPort = 1 << 16 // sorts after every real port

// sortName is what SortName sorts n by, the text the TUI's name column draws for it: the
// container's name for a row drawn as a container, `unknown` for the PID 0 owner, else the
// process's Label.
func (n *node) sortName() string {
	r := n.row
	switch {
	case r.Container != nil:
		return r.Container.Name
	case r.Process.PID == 0:
		return "unknown"
	}
	return r.Process.Label()
}

// here reports whether g is the project devdash was run from.
func (g *group) here() bool { return g.header.Project != nil && g.header.Project.Here }

func containerGroup(c *Container) RowKey {
	if c != nil && c.ComposeProject != "" {
		return RowKey{Header: GroupCompose, Group: c.ComposeProject}
	}
	return RowKey{Header: GroupContainers}
}

// lastRank puts project and compose groups first (0), then `containers`, then `other`.
func lastRank(k GroupKind) GroupKind {
	if k < GroupContainers {
		return 0
	}
	return k
}

// tree links each node to its parent within g and returns the roots, unsorted (order).
func (g *group) tree() []*node {
	byPID := make(map[int]*node, len(g.nodes))
	for _, n := range g.nodes {
		if p := n.row.Process; p != nil && p.PID != 0 {
			byPID[p.PID] = n
		}
	}
	var roots []*node
	for _, n := range g.nodes {
		if parent := n.parent(byPID); parent != nil {
			parent.children = append(parent.children, n)
		} else {
			roots = append(roots, n)
		}
	}
	return roots
}

// order sorts nodes, and below each shown one the children drawn under its row. With
// opts.Fold, a shown node that starts a chain is folded first, so that it sorts by its row.
func order(nodes []*node, opts *ViewOptions) {
	for _, n := range nodes {
		if n.show && opts.Fold {
			n.fold(opts)
		}
	}
	slices.SortFunc(nodes, func(a, b *node) int { return compareNodes(a, b, opts.Sort) })
	for _, n := range nodes {
		switch {
		case n.last != nil:
			order(n.last.children, opts)
		case n.show:
			order(n.children, opts)
		}
	}
}

// fold sets n's links and last when n starts a chain, and gives n its row's sort keys: the
// last node's, and in name mode the chain's label as the TUI draws it without a filter: the
// links ShowAll shows (whatever Search shows), then the last node's own.
func (n *node) fold(opts *ViewOptions) {
	collapsed, last := opts.Collapsed, n
	for c := last.next(collapsed); c != nil; c = last.next(collapsed) {
		n.links = append(n.links, last.row.Process)
		last = c
	}
	if n.links == nil {
		return
	}
	n.last = last
	n.start, n.cpu, n.port = last.start, last.cpu, last.port
	if opts.Sort == SortName {
		var b strings.Builder
		for _, p := range n.links {
			if opts.ShowAll || !p.Hideable() { // the label leaves out the links the view hides (DEV-160)
				b.WriteString(p.Label() + ChainSep)
			}
		}
		n.name = strings.ToLower(b.String() + last.sortName())
	}
}

// next returns n's only shown child when n can be a link of a chain: a process (not the PID 0
// owner) with no listener, no tag and no ContainerID, and not collapsed; else nil.
func (n *node) next(collapsed map[RowKey]bool) *node {
	p := n.row.Process
	if p == nil || p.PID == 0 || len(p.Listeners) > 0 || p.Tags != 0 || p.ContainerID != "" || collapsed[n.row.Key] {
		return nil
	}
	var only *node
	for _, c := range n.children {
		if c.show {
			if only != nil {
				return nil
			}
			only = c
		}
	}
	return only
}

// parent returns the node of n's parent process, or nil when n is a root: a container row, a
// PID 0 row, a parent pid of 0 or 1 (init, launchd), a parent outside the group or not a row,
// or a parent that did not start strictly before n by (start time, pid). The last rule catches
// a reused parent pid and makes cycles impossible.
func (n *node) parent(byPID map[int]*node) *node {
	p := n.row.Process
	if p == nil || p.PID == 0 || p.PPID <= 1 {
		return nil
	}
	par := byPID[p.PPID]
	if par == nil {
		return nil
	}
	pp := par.row.Process
	if c := pp.StartTime.Compare(p.StartTime); c > 0 || c == 0 && pp.PID >= p.PID {
		return nil
	}
	return par
}

// compareNodes orders siblings: by the sort mode, then listeners first, then oldest first,
// then by row key so that equal rows keep a fixed order.
func compareNodes(a, b *node, by SortMode) int {
	var c int
	switch by {
	case SortPort:
		c = cmp.Compare(a.port, b.port)
	case SortCPU:
		c = cmp.Compare(b.cpu, a.cpu) // cmp.Compare sorts NaN lowest, so unknown CPU goes last
	case SortStart:
		c = b.start.Compare(a.start)
	case SortName:
		c = cmp.Compare(a.name, b.name)
	}
	ka, kb := a.row.Key, b.row.Key
	return cmp.Or(c,
		boolCompare(a.port == noPort, b.port == noPort),
		a.start.Compare(b.start),
		cmp.Compare(ka.PID, kb.PID),
		cmp.Compare(ka.ContainerID, kb.ContainerID), // process rows before container-only rows
		cmp.Compare(ka.StartTime, kb.StartTime),
		cmp.Compare(ka.Listener.Proto, kb.Listener.Proto),
		ka.Listener.Addr.Compare(kb.Listener.Addr),
		cmp.Compare(ka.Listener.Port, kb.Listener.Port))
}

// boolCompare orders false before true.
func boolCompare(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// mark sets show and Dimmed over n's subtree and reports whether n is shown.
func (n *node) mark() bool {
	below := false
	for _, c := range n.children {
		below = c.mark() || below
	}
	n.row.Dimmed = n.hidden && below
	n.show = !n.hidden || below
	return n.show
}

// emit appends n and, unless n is collapsed, its shown descendants in preorder. A node that
// starts a chain is appended as its last node's row with the links.
func (n *node) emit(rows []Row, depth int, opts *ViewOptions) []Row {
	if !n.show {
		return rows
	}
	r, below := n.row, n
	if n.last != nil {
		r, below = n.last.row, n.last
		r.Links = n.links
	}
	r.Depth = depth
	rows = append(rows, r)
	if !opts.Collapsed[r.Key] {
		for _, c := range below.children {
			rows = c.emit(rows, depth+1, opts)
		}
	}
	return rows
}

// Filter returns the rows that match query plus the ancestors of each, in order (spec "TUI
// design"). A row matches as Match says; a matching header keeps its whole group. An empty
// query returns rows.
func Filter(rows []Row, query string) []Row {
	if query == "" {
		return rows
	}
	match := matcher(query)
	keep := make([]bool, len(rows))
	var path []int // path[d] is the index of the latest row at depth d
	group := false
	for i, r := range rows {
		path = append(path[:min(r.Depth, len(path))], i)
		m := match(r)
		if r.Depth == 0 {
			group = m
		}
		if !group && !m {
			continue
		}
		for j := len(path) - 1; j >= 0 && !keep[path[j]]; j-- { // a kept row's ancestors are already kept
			keep[path[j]] = true
		}
	}
	var out []Row
	for i, r := range rows {
		if keep[i] {
			out = append(out, r)
		}
	}
	return out
}

// Match reports whether row r itself matches query, as Filter tests each row (Filter adds
// the ancestors of a match and the rows under a matching header). A row matches when query
// is a case-insensitive substring of a process's name, argv or one of its tags (as label or
// JSON name: "cwd deleted" or "cwd_deleted"), a container's name or image, or a header's
// project or compose name. A query of digits only matches listener and published ports by
// prefix ("30" finds 3000 and 3001) and nothing else. An empty query matches every row.
func Match(r Row, query string) bool {
	if query == "" {
		return true
	}
	return matcher(query)(r)
}

// matcher returns Match for a non-empty query, with the query lower-cased once.
func matcher(query string) func(Row) bool {
	q := strings.ToLower(query)
	digits := strings.Trim(q, "0123456789") == ""
	return func(r Row) bool { return matches(r, q, digits) }
}

func matches(r Row, q string, digits bool) bool {
	for _, l := range r.Links { // a folded row matches when one of its processes would (DEV-157)
		if matches(Row{Process: l}, q, digits) {
			return true
		}
	}
	var texts []string
	var ports []uint16
	if r.Project != nil {
		texts = append(texts, r.Project.Name)
	}
	if r.Key.Header == GroupCompose {
		texts = append(texts, r.Key.Group)
	}
	if p := r.Process; p != nil {
		texts = append(texts, p.Name, strings.Join(p.Argv, " "))
		texts = append(texts, p.Tags.Labels()...)
		texts = append(texts, p.Tags.Names()...)
		for _, l := range p.Listeners {
			ports = append(ports, l.Port)
		}
	}
	if c := r.Container; c != nil {
		texts = append(texts, c.Name, c.Image)
		for _, m := range c.Ports {
			if m.HostPort != 0 {
				ports = append(ports, m.HostPort)
			}
		}
	}
	if digits {
		return slices.ContainsFunc(ports, func(p uint16) bool { return strings.HasPrefix(strconv.Itoa(int(p)), q) })
	}
	return slices.ContainsFunc(texts, func(t string) bool { return strings.Contains(strings.ToLower(t), q) })
}
