package model

import (
	"cmp"
	"maps"
	"math"
	"net/netip"
	"path"
	"slices"
	"strings"
	"time"
)

// Raw is one collector sample, as produced by collector.Collector (collector.Result is an alias).
// Processes carry only the collected fields: PID, PPID, UID, StartTime, Name, Argv, Cwd,
// CwdDeleted, CPUTime, RSSBytes and the argv/cwd/cpu/mem bits of Unknown; Build derives the rest.
// Host.OS picks the platform's orphaned rule (Tag).
type Raw struct {
	TakenAt   time.Time // when sampling started; the wall clock for CPUPercent
	Host      Host
	Processes []Process
	Listeners []RawListener
	Warnings  []Warning // may repeat a Code; Build merges them
	Timings   Timing
	// OwnerHint replaces the listener_owner_unreadable hint when the collector knows sudo
	// cannot help (devdash already runs as root on Linux); "" keeps "run with sudo to see owners".
	OwnerHint string
	// OwnerSudo marks the listener_owner_unreadable warning as fixed by sudo (Warning.Sudo):
	// the collector is not root and root would see the owners it could not.
	OwnerSudo bool
}

// RawListener is a listening socket with the pid that owns it, 0 when no owner was readable.
// Collectors report a socket shared across fork once, owned by the lowest pid.
type RawListener struct {
	Proto string
	Addr  netip.Addr
	Port  uint16
	PID   int
}

// commCut is the shortest length at which a kernel name may have been cut: Linux keeps 15
// bytes of comm (TASK_COMM_LEN 16 with the NUL), macOS 16 of p_comm (MAXCOMLEN).
const commCut = 15

// NeedsArgv reports whether a process with kernel name name needs its argv read even when the
// collector limits argv reads over the process limit, because a model rule reads it:
//   - a name at least commCut long may have been cut by the kernel, and fullName restores it
//     only from argv[0] or the script an interpreter runs;
//   - a name that MayBeRuntime is a runtime process only by the basename of argv[0] when the
//     name is cut or re-exec'd (pasta as pasta.avx2), and without it IsContainerRuntime is
//     false: Resolve puts the process in a project and kill does not refuse it;
//   - systemd is a subreaper only by the --user in its argv (userSubreaper), and without it
//     Tag never marks its children orphaned.
func NeedsArgv(name string) bool {
	return len(name) >= commCut || MayBeRuntime(name) || name == "systemd"
}

// fullName undoes the kernel's truncation of Name: when Name is at least commCut long and is
// a strict prefix of the basename of a known argv[0] (its program; a login shell's "-" dropped,
// as Classify does), that basename is the name; else, likewise, the basename of the script an
// interpreter or shell runs (toolArg), since Linux names a #! script's process after the
// script while argv[0] is the interpreter (DEV-175). Otherwise Name is kept, so a rewritten
// title ("nginx: master process ...") or a symlinked argv[0] never invents a name.
func fullName(p Process) string {
	prog := program(p)
	if len(p.Name) < commCut || p.Unknown&FieldArgv != 0 || prog == "" {
		return p.Name
	}
	names := []string{strings.TrimPrefix(path.Base(prog), "-")}
	if i, tool, module := toolArg(baseName(prog), p.Argv[1:]); i >= 0 && !module {
		names = append(names, path.Base(tool))
	}
	for _, b := range names {
		if len(b) > len(p.Name) && strings.HasPrefix(b, p.Name) {
			return b
		}
	}
	return p.Name
}

// unknownOwner is every field of the PID 0 pseudo-process: nothing about it is readable.
const unknownOwner = FieldOwner | FieldArgv | FieldCwd | FieldCPU | FieldMem

// Build turns one collector sample into a Snapshot. prev is the previous snapshot (the zero
// value on the first tick) and only feeds CPUPercent; containers is the latest Docker list
// (nil without Docker). Build never mutates raw, prev or containers; the snapshot's Processes
// slice and Timing map are fresh (Argv slices are shared read-only), so raw may be reused.
//
// Steps: copy processes; attach each listener to its owner, once per (proto, addr, port); give every distinct listener with
// no owner in raw.Processes its own PID 0 "unknown" process; compute CPUPercent; merge
// warnings by Code; call r.Resolve, Classify, Reconcile and Tag, in that order; then add one
// listener_owner_unreadable warning counting the PID 0 rows no container explains.
func Build(raw Raw, prev Snapshot, containers []Container, r *Resolver) Snapshot {
	procs := make([]Process, len(raw.Processes), len(raw.Processes)+len(raw.Listeners))
	byPID := make(map[int]int, len(raw.Processes))
	for i, p := range raw.Processes {
		p.Listeners, p.Kind, p.ProjectID, p.ContainerID, p.Tags = nil, KindOther, "", "", 0
		p.Name = fullName(p)
		procs[i] = p
		byPID[p.PID] = i
	}

	seen := map[Listener]bool{}
	for _, rl := range raw.Listeners {
		l := Listener{Proto: rl.Proto, Addr: rl.Addr, Port: rl.Port}
		if i, ok := byPID[rl.PID]; ok && rl.PID != 0 {
			// One entry per address: SO_REUSEPORT sockets of one process are one listener to the user.
			if !slices.Contains(procs[i].Listeners, l) {
				procs[i].Listeners = append(procs[i].Listeners, l)
			}
			continue
		}
		// No owner, or an owner that is not in the process list (exited, hidden): one row per
		// distinct socket address, so SO_REUSEPORT duplicates collapse and row keys stay unique.
		if !seen[l] {
			seen[l] = true
			procs = append(procs, Process{Name: "unknown", Listeners: []Listener{l}, Unknown: unknownOwner})
		}
	}

	cpuPercent(procs, prev, raw.TakenAt)
	warnings := mergeWarnings(raw.Warnings)
	timing := Timing{}
	maps.Copy(timing, raw.Timings)
	t := time.Now()
	projects := r.Resolve(procs)
	timing["projects"] = time.Since(t)
	for i := range procs {
		procs[i].Kind = Classify(procs[i])
	}
	procs = Reconcile(procs, containers)
	Tag(procs, raw.Host.OS)
	// The PID 0 rows are the ones appended above, one listener each. A row Reconcile matched to
	// a container (root's docker-proxy seen by a user) is explained, and sudo would only show
	// the proxy, so only the others count (DEV-77).
	unowned := 0
	for _, p := range procs[len(raw.Processes):] {
		if p.Listeners[0].ContainerID == "" {
			unowned++
		}
	}
	if unowned > 0 {
		hint := cmp.Or(raw.OwnerHint, "run with sudo to see owners")
		warnings = append(warnings, Warning{Code: "listener_owner_unreadable", Count: unowned, Hint: hint, Sudo: raw.OwnerSudo})
	}

	return Snapshot{
		SchemaVersion: 1,
		TakenAt:       raw.TakenAt,
		Host:          raw.Host,
		Processes:     procs,
		Projects:      projects,
		Containers:    containers,
		Warnings:      warnings,
		Timing:        timing,
	}
}

// cpuPercent sets CPUPercent on every element of procs, sampled at now: the CPU time used since
// prev by the same process (pid and start time) over the wall time since prev, NaN when either
// sample's CPU time is unknown, the process is not in prev, or the clock did not move forward.
func cpuPercent(procs []Process, prev Snapshot, now time.Time) {
	type id struct {
		pid   int
		start int64
	}
	prevCPU := make(map[id]time.Duration, len(prev.Processes))
	for _, p := range prev.Processes {
		if p.PID != 0 && p.Unknown&FieldCPU == 0 {
			prevCPU[id{p.PID, p.StartTime.UnixNano()}] = p.CPUTime
		}
	}
	wall := now.Sub(prev.TakenAt)
	for i := range procs {
		p := &procs[i]
		p.CPUPercent = math.NaN()
		before, ok := prevCPU[id{p.PID, p.StartTime.UnixNano()}]
		if ok && wall > 0 && p.Unknown&FieldCPU == 0 && p.CPUTime >= before {
			p.CPUPercent = float64(p.CPUTime-before) / float64(wall) * 100
		}
	}
}

// mergeWarnings returns one warning per Code, in order of first appearance, with the counts
// summed and the first one's hint; ws is not modified.
func mergeWarnings(ws []Warning) []Warning {
	var warnings []Warning
	at := map[string]int{}
	for _, w := range ws {
		if i, ok := at[w.Code]; ok {
			warnings[i].Count += w.Count
			continue
		}
		at[w.Code] = len(warnings)
		warnings = append(warnings, w)
	}
	return warnings
}
