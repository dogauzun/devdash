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
	Dimmed bool // hidden by the view (shell, editor) but drawn to keep a visible descendant connected
}

// SortMode orders the roots within each group (the `s` key cycles through them).
type SortMode uint8

// Sort modes.
const (
	SortDefault SortMode = iota // listeners first, then start time
	SortPort                    // lowest port first
	SortCPU                     // highest CPU percent first
	SortStart                   // most recently started first
)

// ViewOptions are the view toggles Flatten honours. The text filter is not one: it runs on
// the returned rows (see Row.Depth).
type ViewOptions struct {
	ShowAll        bool            // `a` / --all: show shells and editors
	HideContainers bool            // `d`: drop container rows (processes with a ContainerID and containers without one)
	Sort           SortMode        // `s`
	Collapsed      map[RowKey]bool // collapsed headers and tree nodes
}

// Flatten turns a snapshot into display order (spec "Process tree and kinds" and "TUI design"):
// one header per group, ordered by most recent activity with `containers` and `other` last,
// each followed by its process tree by ppid with roots sorted by opts.Sort; containers with no
// process become rows under their compose project or `containers`; hidden kinds are dropped
// unless ShowAll or needed to connect a visible descendant (then Dimmed); children of collapsed
// keys are skipped. s must not be modified while the rows are in use.
//
// A process whose argv is known to be empty (not merely unreadable) and that has no listener
// is never a row. A group with no row to show
// is omitted. Equal sort keys fall back to the row key, so the order never depends on the
// order of s.Processes.
func Flatten(s Snapshot, opts ViewOptions) []Row {
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
			hidden: !opts.ShowAll && len(p.Listeners) == 0 && (p.Kind == KindShell || p.Kind == KindEditor)} // a listener is never hidden
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

	ordered := slices.SortedFunc(maps.Values(groups), func(a, b *group) int {
		ka, kb := a.header.Key, b.header.Key
		return cmp.Or(
			cmp.Compare(lastRank(ka.Header), lastRank(kb.Header)),
			b.active.Compare(a.active),
			cmp.Compare(ka.Header, kb.Header),
			cmp.Compare(ka.Group, kb.Group))
	})
	var rows []Row
	for _, g := range ordered {
		roots := g.tree(opts.Sort)
		shown := false
		for _, n := range roots {
			shown = n.mark() || shown
		}
		if !shown {
			continue
		}
		rows = append(rows, g.header)
		if opts.Collapsed[g.header.Key] {
			continue
		}
		for _, n := range roots {
			rows = n.emit(rows, 1, opts.Collapsed)
		}
	}
	return rows
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
	port     int // lowest listener or published port, noPort when none
	hidden   bool
	show     bool // not hidden, or has a descendant that is not hidden
	children []*node
}

const noPort = 1 << 16 // sorts after every real port

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

// tree links each node to its parent within g and returns the sorted roots.
func (g *group) tree(by SortMode) []*node {
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
	order := func(a, b *node) int { return compareNodes(a, b, by) }
	for _, n := range g.nodes {
		slices.SortFunc(n.children, order)
	}
	slices.SortFunc(roots, order)
	return roots
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

// emit appends n and, unless n is collapsed, its shown descendants in preorder.
func (n *node) emit(rows []Row, depth int, collapsed map[RowKey]bool) []Row {
	if !n.show {
		return rows
	}
	r := n.row
	r.Depth = depth
	rows = append(rows, r)
	if !collapsed[r.Key] {
		for _, c := range n.children {
			rows = c.emit(rows, depth+1, collapsed)
		}
	}
	return rows
}

// Filter returns the rows that match query plus the ancestors of each, in order (spec "TUI
// design"). A row matches when query is a case-insensitive substring of a process's name or
// argv, a container's name or image, or a header's project or compose name; a matching
// header keeps its whole group. A query of digits only matches listener and published ports
// by prefix ("30" finds 3000 and 3001) and nothing else. An empty query returns rows.
func Filter(rows []Row, query string) []Row {
	if query == "" {
		return rows
	}
	q := strings.ToLower(query)
	digits := strings.Trim(q, "0123456789") == ""
	keep := make([]bool, len(rows))
	var path []int // path[d] is the index of the latest row at depth d
	group := false
	for i, r := range rows {
		path = append(path[:min(r.Depth, len(path))], i)
		m := matches(r, q, digits)
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

func matches(r Row, q string, digits bool) bool {
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
