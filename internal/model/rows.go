package model

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
// Owned by DEV-17; placeholder: one row per process in snapshot order, depth 0, no headers.
func Flatten(s Snapshot, opts ViewOptions) []Row {
	rows := make([]Row, len(s.Processes))
	for i := range s.Processes {
		rows[i] = Row{Key: s.Processes[i].Key(), Process: &s.Processes[i]}
	}
	return rows
}
