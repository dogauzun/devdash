// Package model holds the snapshot types and the pure functions that build them:
// assembly, project resolution, kinds, Docker reconciliation and row flattening.
// It depends on the standard library only.
package model

import (
	"net/netip"
	"time"
)

// Snapshot is an immutable value produced once per tick; the TUI, the JSON output and the
// actions all derive from it. Nothing in it is mutated after Build returns.
type Snapshot struct {
	SchemaVersion int // 1
	TakenAt       time.Time
	Host          Host
	Processes     []Process
	Projects      []Project
	Containers    []Container
	Warnings      []Warning // deduplicated by Code
	Timing        Timing
}

// Host describes the machine and the user devdash runs as.
type Host struct {
	OS, Arch, Hostname string
	UID                int // effective uid
}

// Process is one process, or the PID 0 "unknown" pseudo-process that holds a listener with no
// readable owner. Identity is (PID, StartTime); PIDs are reused.
type Process struct {
	PID, PPID   int
	StartTime   time.Time
	UID         int
	User        string
	Name        string // comm on Linux, p_comm on macOS; Build replaces a cut-short one with argv[0]'s basename
	Argv        []string
	Cwd         string        // "" when unreadable
	CwdDeleted  bool          // the cwd was removed (collected; spec "Release 1.0", Tags); false when unknown
	CPUTime     time.Duration // cumulative user+system, as collected; input to CPUPercent
	CPUPercent  float64       // delta between consecutive samples of the same identity; NaN on the first
	RSSBytes    uint64
	Listeners   []Listener
	Kind        Kind
	ProjectID   string   // Project.ID or ""
	ContainerID string   // set when every listener this process holds is the same container's (Reconcile)
	Unknown     FieldSet // fields that could not be read
	Tags        TagSet   // facts that suggest a leftover, computed per snapshot by Build
}

// Listener is one listening TCP socket.
type Listener struct {
	Proto       string     // "tcp4" | "tcp6"
	Addr        netip.Addr // bind address; unspecified means every interface
	Port        uint16
	ContainerID string // the container whose published port this is (Reconcile), "" otherwise
}

// Project is one git repository that at least one process belongs to.
type Project struct {
	ID       string // repository root path, also the group key
	Root     string
	Name     string // basename of Root, or of MainRepo for linked worktrees
	Branch   string // "" when detached; then ShortSHA is set
	ShortSHA string
	Worktree bool
	MainRepo string // for linked worktrees only
	Here     bool   // the repository devdash was run from (spec "Release 1.0", Here)
}

// Container is one Docker container as reported by the Engine API.
type Container struct {
	ID, Name, Image string
	State           string // running, paused, restarting ...
	ComposeProject  string // label com.docker.compose.project, "" when absent
	ComposeService  string
	Ports           []PortMapping
}

// PortMapping is one published container port.
type PortMapping struct {
	HostIP        netip.Addr // invalid when Docker reports none
	HostPort      uint16
	ContainerPort uint16
	Proto         string // "tcp" | "udp"
}

// Warning is one deduplicated degraded-mode condition, shown in the footer and in --json.
type Warning struct {
	Code  string // stable, snake_case, e.g. "listener_owner_unreadable"
	Count int
	Hint  string
}

// Timing holds per-source durations keyed by source name ("proctable", "listeners", "projects", ...).
type Timing map[string]time.Duration

// Kind is the role of a process, assigned by Classify and by Reconcile.
type Kind uint8

// Kinds; the zero value is KindOther.
const (
	KindOther Kind = iota
	KindServer
	KindContainer
	KindAgent
	KindTest
	KindWatcher
	KindShell
	KindEditor
)

var kindNames = [...]string{"other", "server", "container", "agent", "test", "watcher", "shell", "editor"}

func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return "other"
}

// FieldSet is a set of process fields that could not be read.
type FieldSet uint8

// Fields, in the order Names lists them.
const (
	FieldOwner FieldSet = 1 << iota // the listener's owning process
	FieldArgv
	FieldCwd
	FieldCPU
	FieldMem
)

var fieldNames = [...]string{"owner", "argv", "cwd", "cpu", "mem"}

// Names lists the set fields by name in a stable order (owner, argv, cwd, cpu, mem);
// never nil, so an empty set encodes as [].
func (s FieldSet) Names() []string {
	names := []string{}
	for i, n := range fieldNames {
		if s&(1<<i) != 0 {
			names = append(names, n)
		}
	}
	return names
}

// TagSet is a set of facts that suggest a process was left over (spec "Release 1.0", Tags).
// Tags state facts; none of them says a process is safe to kill.
type TagSet uint8

// Tags, in the order Names and Labels list them (the spec's tag table).
const (
	TagOrphaned   TagSet = 1 << iota // its parent exited
	TagCwdDeleted                    // its working directory is gone
)

var (
	tagNames  = [...]string{"orphaned", "cwd_deleted"}
	tagLabels = [...]string{"orphaned", "cwd deleted"}
)

// Names lists the set tags by their JSON name (orphaned, cwd_deleted); never nil, so an
// empty set encodes as [].
func (s TagSet) Names() []string { return s.list(tagNames[:]) }

// Labels lists the set tags as people read them (orphaned, cwd deleted), in the same order.
func (s TagSet) Labels() []string { return s.list(tagLabels[:]) }

func (s TagSet) list(words []string) []string {
	out := []string{}
	for i, w := range words {
		if s&(1<<i) != 0 {
			out = append(out, w)
		}
	}
	return out
}
