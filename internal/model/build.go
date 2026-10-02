package model

import (
	"maps"
	"math"
	"net/netip"
	"slices"
	"time"
)

// Raw is one collector sample, as produced by collector.Collector (collector.Result is an alias).
// Processes carry only the collected fields: PID, PPID, UID, StartTime, Name, Argv, Cwd,
// CPUTime, RSSBytes and the argv/cwd/cpu/mem bits of Unknown; Build derives the rest.
type Raw struct {
	TakenAt   time.Time // when sampling started; the wall clock for CPUPercent
	Host      Host
	Processes []Process
	Listeners []RawListener
	Warnings  []Warning // may repeat a Code; Build merges them
	Timings   Timing
}

// RawListener is a listening socket with the pid that owns it, 0 when no owner was readable.
// Collectors report a socket shared across fork once, owned by the lowest pid.
type RawListener struct {
	Proto string
	Addr  netip.Addr
	Port  uint16
	PID   int
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
// warnings by Code; then call r.Resolve, Classify and Reconcile, in that order.
func Build(raw Raw, prev Snapshot, containers []Container, r *Resolver) Snapshot {
	procs := make([]Process, len(raw.Processes), len(raw.Processes)+len(raw.Listeners))
	byPID := make(map[int]int, len(raw.Processes))
	for i, p := range raw.Processes {
		p.Listeners, p.Kind, p.ProjectID, p.ContainerID = nil, KindOther, "", ""
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
	wall := raw.TakenAt.Sub(prev.TakenAt)
	for i := range procs {
		p := &procs[i]
		p.CPUPercent = math.NaN()
		before, ok := prevCPU[id{p.PID, p.StartTime.UnixNano()}]
		if ok && wall > 0 && p.Unknown&FieldCPU == 0 && p.CPUTime >= before {
			p.CPUPercent = float64(p.CPUTime-before) / float64(wall) * 100
		}
	}

	var warnings []Warning
	at := map[string]int{}
	for _, w := range raw.Warnings {
		if i, ok := at[w.Code]; ok {
			warnings[i].Count += w.Count
			continue
		}
		at[w.Code] = len(warnings)
		warnings = append(warnings, w)
	}
	if n := len(seen); n > 0 {
		warnings = append(warnings, Warning{Code: "listener_owner_unreadable", Count: n, Hint: "run with sudo to see owners"})
	}

	timing := Timing{}
	maps.Copy(timing, raw.Timings)
	t := time.Now()
	projects := r.Resolve(procs)
	timing["projects"] = time.Since(t)
	for i := range procs {
		procs[i].Kind = Classify(procs[i])
	}
	procs = Reconcile(procs, containers)

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
