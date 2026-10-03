package model

import "slices"

// Tag sets Tags on every element of procs in place (Build passes its own fresh slice), from
// the collected CwdDeleted, each process's parent in procs and goos, the sampled host's OS
// (spec "Release 1.0", Tags). It runs after Resolve and Reconcile, whose ProjectID and
// ContainerID it reads.
//
// cwd_deleted is the collector's finding. orphaned is ppid 1 (init, or launchd on macOS) or,
// on Linux, a parent that is the user's own `systemd --user`, which adopts the orphans of its
// session as a subreaper. It is set only on a process in a project or with a deleted cwd:
// system services and session agents have the same parents, and tagging them would bury the
// signal. PID 0 rows and container rows never carry tags: the first is no process anyone can
// read, the second is Docker's plumbing, whose parent says nothing about the container.
func Tag(procs []Process, goos string) {
	byPID := make(map[int]int, len(procs))
	for i, p := range procs {
		if p.PID != 0 {
			byPID[p.PID] = i
		}
	}
	for i := range procs {
		p := &procs[i]
		p.Tags = 0
		if p.PID == 0 || p.ContainerID != "" {
			continue
		}
		if p.CwdDeleted {
			p.Tags |= TagCwdDeleted
		}
		if p.ProjectID == "" && !p.CwdDeleted {
			continue
		}
		orphaned := p.PPID == 1
		if j, ok := byPID[p.PPID]; ok && goos == "linux" && !orphaned {
			orphaned = userSubreaper(procs[j], p.UID)
		}
		if orphaned {
			p.Tags |= TagOrphaned
		}
	}
}

// userSubreaper reports whether parent is uid's `systemd --user`. A system manager that is not
// pid 1 (systemd inside a container) or another user's manager adopts nothing of this user's
// session, so neither counts; an unread argv cannot show --user and does not count either.
func userSubreaper(parent Process, uid int) bool {
	return parent.Name == "systemd" && parent.UID == uid && len(parent.Argv) > 1 && slices.Contains(parent.Argv[1:], "--user")
}
