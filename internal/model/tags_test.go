package model

import (
	"net/netip"
	"testing"
)

func TestTag(t *testing.T) {
	const me, other = 1000, 1001
	// p is a process of uid me in project /repo with a live cwd; each case changes what it tests.
	p := func(pid, ppid int) Process {
		return Process{PID: pid, PPID: ppid, UID: me, Name: "node", Argv: []string{"node", "server.js"}, ProjectID: "/repo"}
	}
	sd := func(pid, ppid, uid int, argv ...string) Process {
		return Process{PID: pid, PPID: ppid, UID: uid, Name: "systemd", Argv: argv}
	}
	userManager := sd(900, 1, me, "/usr/lib/systemd/systemd", "--user")
	with := func(q Process, f func(*Process)) Process { f(&q); return q }

	tests := []struct {
		name   string
		goos   string
		parent *Process // in the snapshot alongside the process, when set
		proc   Process
		want   TagSet
	}{
		{"linux ppid 1 in a project", "linux", nil, p(10, 1), TagOrphaned},
		{"darwin ppid 1 (launchd) in a project", "darwin", nil, p(10, 1), TagOrphaned},
		{"linux live parent", "linux", &Process{PID: 5, PPID: 1, UID: me, Name: "zsh"}, p(10, 5), 0},
		{"darwin live parent", "darwin", &Process{PID: 5, PPID: 1, UID: me, Name: "zsh"}, p(10, 5), 0},
		{"parent not in the snapshot", "linux", nil, p(10, 5), 0},
		{"linux systemd --user subreaper", "linux", &userManager, p(10, 900), TagOrphaned},
		{"linux systemd --user of another uid", "linux", new(sd(900, 1, other, "/usr/lib/systemd/systemd", "--user")), p(10, 900), 0},
		{"linux systemd without --user (system manager not pid 1)", "linux", new(sd(900, 0, me, "/sbin/init")), p(10, 900), 0},
		{"linux systemd --user argv unknown", "linux", new(with(sd(900, 1, me), func(q *Process) { q.Unknown = FieldArgv })), p(10, 900), 0},
		{"linux --user only as argv[0]", "linux", new(sd(900, 1, me, "--user")), p(10, 900), 0},
		{"linux --user parent named otherwise", "linux", new(with(userManager, func(q *Process) { q.Name = "systemd-logind" })), p(10, 900), 0},
		{"darwin ignores a systemd --user parent", "darwin", &userManager, p(10, 900), 0},
		{"ppid 1 outside any project, live cwd", "linux", nil, with(p(10, 1), func(q *Process) { q.ProjectID = "" }), 0},
		{"darwin ppid 1 outside any project, live cwd", "darwin", nil, with(p(10, 1), func(q *Process) { q.ProjectID = "" }), 0},
		{"subreaper child outside any project", "linux", &userManager, with(p(10, 900), func(q *Process) { q.ProjectID = "" }), 0},
		{"ppid 1, cwd deleted, no project", "linux", nil, with(p(10, 1), func(q *Process) { q.ProjectID, q.CwdDeleted = "", true }), TagOrphaned | TagCwdDeleted},
		{"darwin ppid 1, cwd deleted, no project", "darwin", nil, with(p(10, 1), func(q *Process) { q.ProjectID, q.CwdDeleted = "", true }), TagOrphaned | TagCwdDeleted},
		{"cwd deleted, live parent", "linux", &Process{PID: 5, PPID: 1, UID: me, Name: "zsh"}, with(p(10, 5), func(q *Process) { q.CwdDeleted = true }), TagCwdDeleted},
		{"cwd deleted, no project, live parent", "darwin", &Process{PID: 5, PPID: 1, UID: me, Name: "zsh"}, with(p(10, 5), func(q *Process) { q.ProjectID, q.CwdDeleted = "", true }), TagCwdDeleted},
		{"PID 0 row", "linux", nil, Process{Name: "unknown", PPID: 1, CwdDeleted: true, ProjectID: "/repo", Unknown: unknownOwner}, 0},
		{"container row", "linux", nil, with(p(10, 1), func(q *Process) { q.ContainerID, q.CwdDeleted = "abc", true }), 0},
		{"unknown OS keeps the ppid 1 rule", "", nil, p(10, 1), TagOrphaned},
		{"stale tags are replaced", "linux", &Process{PID: 5, PPID: 1, UID: me, Name: "zsh"}, with(p(10, 5), func(q *Process) { q.Tags = TagOrphaned | TagCwdDeleted }), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			procs := []Process{tt.proc}
			if tt.parent != nil {
				procs = append(procs, *tt.parent)
			}
			Tag(procs, tt.goos)
			if got := procs[0].Tags; got != tt.want {
				t.Errorf("Tags = %v, want %v", got.Names(), tt.want.Names())
			}
		})
	}
}

// A PID 0 row is nobody's parent, even for a process whose ppid reads 0.
func TestTagPID0NotAParent(t *testing.T) {
	procs := []Process{
		{Name: "unknown", Unknown: unknownOwner, Listeners: []Listener{{Proto: "tcp4", Addr: netip.IPv4Unspecified(), Port: 22}}},
		{PID: 10, PPID: 0, UID: 0, Name: "node", ProjectID: "/repo"},
	}
	Tag(procs, "linux")
	if procs[0].Tags != 0 || procs[1].Tags != 0 {
		t.Errorf("tags %v %v", procs[0].Tags.Names(), procs[1].Tags.Names())
	}
}

// Build tags after Resolve, Classify and Reconcile, from the sample's host OS: the project a
// process resolves to and the container a proxy is matched to decide whether it is tagged.
func TestBuildTags(t *testing.T) {
	base := tmp(t)
	repo := mkrepo(t, base, "shop", "main")
	raw := Raw{
		TakenAt: t0,
		Host:    Host{OS: "linux", UID: 1000},
		Processes: []Process{
			{PID: 900, PPID: 1, UID: 1000, StartTime: start, Name: "systemd", Argv: []string{"/usr/lib/systemd/systemd", "--user"}, Cwd: base},
			{PID: 10, PPID: 900, UID: 1000, StartTime: start, Name: "node", Argv: []string{"node"}, Cwd: repo},
			{PID: 11, PPID: 1, UID: 1000, StartTime: start, Name: "sshd", Argv: []string{"sshd"}, Cwd: "/"},
			{PID: 12, PPID: 1, UID: 0, StartTime: start, Name: "docker-proxy", Argv: []string{"docker-proxy"}, Cwd: "/", CwdDeleted: true},
		},
		Listeners: []RawListener{{"tcp4", any4, 8080, 12}, {"tcp4", any4, 22, 0}},
	}
	containers := []Container{{ID: "abc", Name: "web", Ports: []PortMapping{{HostIP: any4, HostPort: 8080, ContainerPort: 80, Proto: "tcp"}}}}
	s := Build(raw, Snapshot{}, containers, NewResolver("", []string{base}))
	got := map[int]TagSet{}
	for _, p := range s.Processes {
		got[p.PID] |= p.Tags
	}
	want := map[int]TagSet{900: 0, 10: TagOrphaned, 11: 0, 12: 0, 0: 0}
	for pid, w := range want {
		if got[pid] != w {
			t.Errorf("pid %d tags %v, want %v", pid, got[pid].Names(), w.Names())
		}
	}
	if s.Processes[1].ProjectID == "" || s.Processes[3].ContainerID == "" {
		t.Fatalf("fixture: node project %q, proxy container %q", s.Processes[1].ProjectID, s.Processes[3].ContainerID)
	}

	raw.Host.OS = "darwin" // the subreaper rule is Linux's
	s = Build(raw, Snapshot{}, containers, NewResolver("", []string{base}))
	if s.Processes[1].Tags != 0 {
		t.Errorf("darwin: node tags %v, want none", s.Processes[1].Tags.Names())
	}
}
