package model

// RowKey identifies a row across snapshots; the TUI stores selection and expansion by it,
// never by index. A process row is keyed by (PID, StartTime); the PID 0 "unknown" rows, which
// share PID and StartTime, add their one listener; a header row is keyed by Project alone.
type RowKey struct {
	PID       int
	StartTime int64    // StartTime.UnixNano()
	Listener  Listener // PID 0 rows only
	Project   string   // header rows only
}

// Key returns the row key of p.
func (p Process) Key() RowKey {
	k := RowKey{PID: p.PID, StartTime: p.StartTime.UnixNano()}
	if p.PID == 0 && len(p.Listeners) > 0 {
		k.Listener = p.Listeners[0]
	}
	return k
}

// Row is one line of the table both the TUI and the CLI render: a project header or a process.
type Row struct {
	Key     RowKey
	Project *Project // header rows; points into Snapshot.Projects
	Process *Process // process rows; points into Snapshot.Processes
	Depth   int      // tree depth within the group, 0 for roots and headers
	Dimmed  bool     // hidden by the view (shell, editor) but drawn to keep a visible descendant connected
}

// ViewOptions are the view toggles Flatten honours.
type ViewOptions struct {
	ShowAll   bool            // `a` / --all: show shells and editors
	Collapsed map[RowKey]bool // collapsed headers and tree nodes
}

// Flatten turns a snapshot into display order (spec "Process tree and kinds" and "TUI design"):
// one header per group, ordered by most recent activity with `containers` and `other` last,
// each followed by its process tree by ppid; hidden kinds are dropped unless ShowAll or needed
// to connect a visible descendant (then Dimmed); children of collapsed keys are skipped.
// Rows point into s, which must not be modified.
//
// Owned by DEV-17; placeholder: one row per process in snapshot order, depth 0, no headers.
func Flatten(s Snapshot, opts ViewOptions) []Row {
	rows := make([]Row, len(s.Processes))
	for i := range s.Processes {
		rows[i] = Row{Key: s.Processes[i].Key(), Process: &s.Processes[i]}
	}
	return rows
}
