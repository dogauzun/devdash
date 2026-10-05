package model

import (
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// fp is a process for row tests: started startSec seconds after `start`, argv [name].
func fp(pid, ppid, startSec int, name, project string, kind Kind, ports ...uint16) Process {
	p := Process{PID: pid, PPID: ppid, StartTime: start.Add(time.Duration(startSec) * time.Second),
		Name: name, Argv: []string{name}, ProjectID: project, Kind: kind, CPUPercent: math.NaN()}
	for _, port := range ports {
		p.Listeners = append(p.Listeners, Listener{Proto: "tcp4", Addr: lo, Port: port})
	}
	return p
}

var groupNames = map[GroupKind]string{GroupProject: "project", GroupCompose: "compose", GroupContainers: "containers", GroupOther: "other"}

// render draws rows as indented labels: "[project /src/shop]", "api", "zsh (dim)", "proxy@db",
// "ctr:db".
func render(rows []Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		var s string
		switch {
		case r.Key.Header != GroupNone:
			s = "[" + strings.TrimSpace(groupNames[r.Key.Header]+" "+r.Key.Group) + "]"
		case r.Process != nil:
			s = r.Process.Name
			if r.Container != nil {
				s += "@" + r.Container.Name
			}
		default:
			s = "ctr:" + r.Container.Name
		}
		if r.Dimmed {
			s += " (dim)"
		}
		out[i] = strings.Repeat("  ", r.Depth) + s
	}
	return out
}

func shop() []Project { return []Project{{ID: "/src/shop", Root: "/src/shop", Name: "shop"}} }

func TestFlattenTree(t *testing.T) {
	const s = "/src/shop"
	tests := []struct {
		name  string
		procs []Process
		opts  ViewOptions
		want  []string
	}{
		{
			name: "root rules",
			procs: []Process{
				fp(10, 1, 0, "api", s, KindServer, 8080),
				fp(11, 10, 1, "worker", s, KindOther),
				fp(12, 99, 2, "vite", s, KindServer, 5173), // parent in another group
				fp(99, 1, -10, "tmux", "", KindOther),
				fp(13, 500, 3, "job", s, KindOther),      // parent not in the snapshot
				fp(14, 10, -5, "reused", s, KindOther),   // "parent" started later: pid 10 was reused
				fp(15, 0, 4, "orphan", s, KindOther),     // ppid 0
				fp(16, 15, 5, "orphankid", s, KindOther), // child of a ppid-0 root
			},
			want: []string{
				"[project /src/shop]",
				"  api", "    worker", "  vite", "  reused", "  job", "  orphan", "    orphankid",
				"[other]", "  tmux",
			},
		},
		{
			name: "pid 1 parent is never a tree parent",
			procs: []Process{
				fp(1, 0, -100, "init", "", KindOther),
				fp(20, 1, 0, "daemon", "", KindOther),
			},
			want: []string{"[other]", "  init", "  daemon"},
		},
		{
			name: "hidden but connected",
			procs: []Process{
				fp(10, 1, 0, "zsh", s, KindShell),
				fp(11, 10, 1, "vite", s, KindServer, 5173),
				fp(12, 1, 2, "zsh2", s, KindShell), // no visible descendant
				fp(13, 1, 3, "nvim", s, KindEditor),
				fp(14, 13, 4, "gopls", s, KindEditor),
				fp(15, 1, 5, "bash", s, KindShell),
				fp(16, 15, 6, "sh", s, KindShell),
				fp(17, 16, 7, "node", s, KindOther),
			},
			want: []string{"[project /src/shop]", "  zsh (dim)", "    vite", "  bash (dim)", "    sh (dim)", "      node"},
		},
		{
			name: "show all",
			procs: []Process{
				fp(10, 1, 0, "zsh", s, KindShell),
				fp(11, 10, 1, "vite", s, KindServer, 5173),
				fp(13, 1, 3, "nvim", s, KindEditor),
				fp(14, 13, 4, "gopls", s, KindEditor),
			},
			opts: ViewOptions{ShowAll: true},
			want: []string{"[project /src/shop]", "  zsh", "    vite", "  nvim", "    gopls"},
		},
		{
			name: "shells and editors with a listener are never hidden",
			procs: []Process{
				fp(10, 1, 0, "nvim", s, KindEditor, 6666),
				fp(11, 1, 1, "zsh", s, KindShell, 7000),
				fp(12, 1, 2, "gopls", s, KindEditor), // no listener: hidden
			},
			want: []string{"[project /src/shop]", "  nvim", "  zsh"},
		},
		{
			name: "group with only hidden processes is omitted",
			procs: []Process{
				fp(10, 1, 0, "zsh", s, KindShell),
				fp(20, 1, 0, "cron", "", KindOther),
			},
			want: []string{"[other]", "  cron"},
		},
		{
			name: "empty argv: omitted without a listener, kept with one; unknown owner kept",
			procs: []Process{
				{PID: 20, PPID: 1, StartTime: start, Name: "blank"},
				{PID: 21, PPID: 1, StartTime: start, Name: "blanklisten", Listeners: []Listener{{Proto: "tcp4", Addr: lo, Port: 9000}}},
				fp(22, 20, 1, "child", "", KindOther), // parent omitted: a root
				{Name: "unknown", Listeners: []Listener{{Proto: "tcp4", Addr: any4, Port: 22}}, Unknown: unknownOwner, CPUPercent: math.NaN()},
				{Name: "unknown", Listeners: []Listener{{Proto: "tcp6", Addr: any4, Port: 631}}, Unknown: unknownOwner, CPUPercent: math.NaN()},
			},
			want: []string{"[other]", "  unknown", "  unknown", "  blanklisten", "  child"},
		},
		{
			name: "unreadable argv (other uid) is a row and keeps its children",
			procs: []Process{
				{PID: 30, PPID: 1, StartTime: start, Name: "sshd", Unknown: FieldArgv | FieldCwd},
				fp(31, 30, 1, "worker", "", KindOther),
				{PID: 32, PPID: 1, StartTime: start.Add(2 * time.Second), Name: "zsh", Kind: KindShell, Unknown: FieldArgv},
				fp(33, 32, 3, "make", "", KindOther),
				{PID: 34, PPID: 1, StartTime: start.Add(4 * time.Second), Name: "bash", Kind: KindShell, Unknown: FieldArgv}, // hidden by Name, nothing below
				{PID: 35, PPID: 1, StartTime: start, Name: "blank"},                                                          // known-empty argv, no listener
			},
			want: []string{"[other]", "  sshd", "    worker", "  zsh (dim)", "    make"},
		},
		{
			name: "unknown project id goes to other",
			procs: []Process{
				fp(10, 1, 0, "api", "/src/gone", KindServer, 80),
			},
			want: []string{"[other]", "  api"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := render(Flatten(Snapshot{Processes: tt.procs, Projects: shop()}, tt.opts))
			if !slices.Equal(got, tt.want) {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tt.want, "\n"))
			}
		})
	}
}

// TestListeningEditorShown: real Classify output for editors that listen; their ports must
// stay visible and findable in the default view.
func TestListeningEditorShown(t *testing.T) {
	var procs []Process
	for i, c := range []struct {
		argv string
		port uint16
	}{
		{"nvim --listen 127.0.0.1:6666", 6666},
		{"gopls serve -listen=:37374", 37374},
		{"/usr/share/code/code --type=utility --utility-sub-type=node.mojom.NodeService", 5500},
	} {
		p := Process{PID: 10 + i, PPID: 1, StartTime: start, Argv: strings.Fields(c.argv), Listeners: []Listener{{Proto: "tcp4", Addr: lo, Port: c.port}}}
		if p.Kind = Classify(p); p.Kind != KindEditor {
			t.Fatalf("%q classified %v, want editor", c.argv, p.Kind)
		}
		procs = append(procs, p)
	}
	rows := Flatten(Snapshot{Processes: procs}, ViewOptions{})
	for _, port := range []string{"6666", "37374", "5500"} {
		got := Filter(rows, port)
		if len(got) != 2 || got[1].Dimmed {
			t.Errorf("Filter(%q) = %q, want the other header and one undimmed editor row", port, render(got))
		}
	}
}

func TestFlattenRowFields(t *testing.T) {
	snap := Snapshot{Processes: []Process{fp(10, 1, 0, "api", "/src/shop", KindServer, 80)}, Projects: shop()}
	rows := Flatten(snap, ViewOptions{})
	if len(rows) != 2 {
		t.Fatalf("%d rows", len(rows))
	}
	h, p := rows[0], rows[1]
	if h.Key != (RowKey{Header: GroupProject, Group: "/src/shop"}) || h.Project != &snap.Projects[0] || h.Process != nil || h.Depth != 0 {
		t.Errorf("header %+v", h)
	}
	if p.Key != snap.Processes[0].Key() || p.Process != &snap.Processes[0] || p.Project != nil || p.Depth != 1 {
		t.Errorf("process row %+v", p)
	}
}

func TestFlattenSort(t *testing.T) {
	const s = "/src/shop"
	cpu := func(p Process, c float64) Process { p.CPUPercent = c; return p }
	vite := cpu(fp(11, 1, 1, "node", s, KindServer, 8080), 1)
	vite.Argv = []string{"node", "node_modules/.bin/vite"} // labelled `vite (node)`
	procs := []Process{
		cpu(fp(10, 1, 0, "old", s, KindOther), 5),
		vite,
		cpu(fp(12, 1, 2, "db", s, KindServer, 5432, 80), 50),
		fp(13, 1, 3, "New", s, KindOther), // CPU unknown (NaN)
		cpu(fp(14, 12, 4, "kid-a", s, KindOther), 1),
		cpu(fp(15, 12, 5, "kid-b", s, KindServer, 9000), 9),
	}
	tests := []struct {
		mode SortMode
		want string
	}{
		{SortDefault, "node db kid-b kid-a old New"},
		{SortPort, "db kid-b kid-a node old New"}, // portless last, then oldest first
		{SortCPU, "db kid-b kid-a old node New"},
		{SortStart, "New db kid-b kid-a node old"},
		{SortName, "db kid-a kid-b New old node"}, // by label, case-insensitive: `vite (node)` under v
	}
	for _, tt := range tests {
		var got []string
		for _, r := range Flatten(Snapshot{Processes: procs, Projects: shop()}, ViewOptions{Sort: tt.mode})[1:] {
			got = append(got, r.Process.Name)
		}
		if strings.Join(got, " ") != tt.want {
			t.Errorf("sort %d: %v, want %s", tt.mode, got, tt.want)
		}
	}
}

func TestFlattenGroupOrder(t *testing.T) {
	projects := []Project{{ID: "/a", Name: "a"}, {ID: "/b", Name: "b"}, {ID: "/c", Name: "c"}}
	snap := Snapshot{
		Projects: projects,
		Processes: []Process{
			fp(10, 1, 0, "a1", "/a", KindOther),
			fp(11, 1, 5, "a2", "/a", KindOther), // a: latest 5
			fp(12, 1, 9, "b1", "/b", KindOther), // b: latest 9
			fp(13, 1, 5, "c1", "/c", KindOther),
			fp(14, 1, 7, "proxy", "", KindContainer, 5432),
			fp(15, 1, 100, "lone", "", KindOther), // newest, but other is last
			fp(16, 1, 99, "zsh", "/c", KindShell), // hidden processes still count as activity
			fp(17, 1, 50, "proxy2", "", KindContainer, 6379),
		},
		Containers: []Container{
			{ID: "db", Name: "db", ComposeProject: "shop", Ports: []PortMapping{{HostPort: 5432}}},
			{ID: "cache", Name: "cache", Ports: []PortMapping{{HostPort: 6379}}},
		},
	}
	snap.Processes[4].ContainerID = "db"
	snap.Processes[7].ContainerID = "cache"
	var got []string
	for _, r := range Flatten(snap, ViewOptions{}) {
		if r.Depth == 0 {
			got = append(got, groupNames[r.Key.Header]+" "+r.Key.Group)
		}
	}
	want := []string{"project /c", "project /b", "compose shop", "project /a", "containers ", "other "}
	if !slices.Equal(got, want) {
		t.Errorf("groups %q, want %q", got, want)
	}
}

func TestFlattenContainers(t *testing.T) {
	proxy := fp(30, 1, 0, "docker-proxy", "", KindContainer, 5432)
	proxy.ContainerID = "c1"
	unknown := Process{Name: "unknown", Listeners: []Listener{{Proto: "tcp4", Addr: any4, Port: 8081}}, Unknown: unknownOwner, ContainerID: "c5", Kind: KindContainer, CPUPercent: math.NaN()}
	snap := Snapshot{
		Projects: shop(),
		Processes: []Process{
			proxy, unknown,
			fp(10, 1, 0, "api", "/src/shop", KindServer, 8080),
		},
		Containers: []Container{
			{ID: "c1", Name: "db", ComposeProject: "shop", Ports: []PortMapping{{HostPort: 5432, ContainerPort: 5432}}},
			{ID: "c2", Name: "redis", Ports: []PortMapping{{HostPort: 6379, ContainerPort: 6379}}},        // no process, no compose
			{ID: "c3", Name: "worker", ComposeProject: "shop", Ports: []PortMapping{{ContainerPort: 80}}}, // nothing published
			{ID: "c4", Name: "mail", ComposeProject: "shop", Ports: []PortMapping{{HostPort: 1025}}},      // no process
			{ID: "c5", Name: "web", Ports: []PortMapping{{HostPort: 8081}}},                               // root-owned proxy
		},
	}
	// Equal activity: project before compose. Container-only rows have no start time, so the
	// "oldest first" rule puts them first among rows with ports; a process beats a container
	// row on the same PID 0.
	want := []string{
		"[project /src/shop]", "  api",
		"[compose shop]", "  ctr:mail", "  docker-proxy@db",
		"[containers]", "  unknown@web", "  ctr:redis",
	}
	rows := Flatten(snap, ViewOptions{})
	if got := render(rows); !slices.Equal(got, want) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if rows[4].Container != &snap.Containers[0] || rows[4].Process != &snap.Processes[0] || rows[3].Key != (RowKey{ContainerID: "c4"}) || rows[3].Process != nil || rows[3].Container != &snap.Containers[3] {
		t.Errorf("container rows %+v %+v", rows[3], rows[4])
	}
	if got := render(Flatten(snap, ViewOptions{HideContainers: true})); !slices.Equal(got, []string{"[project /src/shop]", "  api"}) {
		t.Errorf("HideContainers: %q", got)
	}
}

func TestFlattenCollapse(t *testing.T) {
	const s = "/src/shop"
	snap := Snapshot{Projects: shop(), Processes: []Process{
		fp(10, 1, 0, "api", s, KindServer, 80),
		fp(11, 10, 1, "worker", s, KindOther),
		fp(12, 11, 2, "grandkid", s, KindOther),
		fp(20, 1, 0, "cron", "", KindOther),
	}}
	tests := []struct {
		collapsed []RowKey
		want      []string
	}{
		{nil, []string{"[project /src/shop]", "  api", "    worker", "      grandkid", "[other]", "  cron"}},
		{[]RowKey{{Header: GroupProject, Group: s}}, []string{"[project /src/shop]", "[other]", "  cron"}},
		{[]RowKey{snap.Processes[1].Key()}, []string{"[project /src/shop]", "  api", "    worker", "[other]", "  cron"}},
		{[]RowKey{snap.Processes[0].Key(), {Header: GroupOther}}, []string{"[project /src/shop]", "  api", "[other]"}},
	}
	for _, tt := range tests {
		opts := ViewOptions{Collapsed: map[RowKey]bool{}}
		for _, k := range tt.collapsed {
			opts.Collapsed[k] = true
		}
		if got := render(Flatten(snap, opts)); !slices.Equal(got, tt.want) {
			t.Errorf("collapsed %v: %q, want %q", tt.collapsed, got, tt.want)
		}
	}
}

// bigSnapshot has n processes in 50 projects plus other, as trees with three children per
// node, and a few containers.
func bigSnapshot(n int) Snapshot {
	var s Snapshot
	for i := range 50 {
		s.Projects = append(s.Projects, Project{ID: fmt.Sprintf("/src/p%02d", i), Name: fmt.Sprintf("p%02d", i)})
	}
	kinds := []Kind{KindOther, KindServer, KindShell, KindEditor, KindTest}
	for i := range n {
		pid := 100 + i
		ppid := 1
		if i%10 != 0 {
			ppid = 100 + i/3
		}
		project := ""
		if i%7 != 0 {
			project = s.Projects[(i/10)%50].ID
		}
		p := fp(pid, ppid, i%500, fmt.Sprintf("proc%d", i), project, kinds[i%len(kinds)])
		p.CPUPercent = float64(i % 13)
		if i%5 == 1 {
			p.Listeners = []Listener{{Proto: "tcp4", Addr: lo, Port: uint16(1000 + i)}}
		}
		s.Processes = append(s.Processes, p)
	}
	for i := range 20 {
		s.Containers = append(s.Containers, Container{ID: fmt.Sprint("c", i), Name: fmt.Sprint("ctr", i),
			ComposeProject: []string{"", "shop"}[i%2], Ports: []PortMapping{{HostPort: uint16(20000 + i)}}})
	}
	return s
}

func TestFlattenDeterministicAndPure(t *testing.T) {
	snap := bigSnapshot(2000)
	before := slices.Clone(snap.Processes)
	for _, opts := range []ViewOptions{{}, {ShowAll: true, Sort: SortCPU}, {Sort: SortPort}, {Sort: SortStart, HideContainers: true}} {
		want := render(Flatten(snap, opts))
		shuffled := snap
		shuffled.Processes = slices.Clone(snap.Processes)
		rand.Shuffle(len(shuffled.Processes), func(i, j int) {
			shuffled.Processes[i], shuffled.Processes[j] = shuffled.Processes[j], shuffled.Processes[i]
		})
		if got := render(Flatten(shuffled, opts)); !slices.Equal(got, want) {
			t.Errorf("opts %+v: order depends on input order", opts)
		}
	}
	if !reflect.DeepEqual(before, snap.Processes) {
		t.Error("Flatten modified the snapshot")
	}
}

func TestFlattenDeepChain(t *testing.T) {
	var procs []Process
	for i := range 5000 {
		procs = append(procs, fp(100+i, 99+i, i, fmt.Sprint("p", i), "", KindOther))
	}
	rows := Flatten(Snapshot{Processes: procs}, ViewOptions{})
	if len(rows) != 5001 || rows[5000].Depth != 5000 {
		t.Errorf("%d rows, last depth %d", len(rows), rows[len(rows)-1].Depth)
	}
}

func BenchmarkFlatten(b *testing.B) {
	snap := bigSnapshot(5000)
	b.ReportAllocs()
	for b.Loop() {
		Flatten(snap, ViewOptions{})
	}
}

func TestFilter(t *testing.T) {
	const s = "/src/shop"
	proxy := fp(30, 1, 0, "docker-proxy", "", KindContainer, 5432)
	proxy.ContainerID = "c1"
	snap := Snapshot{
		Projects: shop(),
		Processes: []Process{
			fp(10, 1, 0, "api", s, KindServer, 8080),
			fp(11, 10, 1, "Worker", s, KindOther),
			fp(12, 11, 2, "grandkid", s, KindOther, 80),
			fp(13, 1, 3, "vite", s, KindServer, 3000),
			fp(20, 1, 0, "cron", "", KindOther),
			proxy,
		},
		Containers: []Container{
			{ID: "c1", Name: "db", Image: "postgres:16", ComposeProject: "store", Ports: []PortMapping{{HostPort: 5432}}},
			{ID: "c2", Name: "redis", Image: "redis:7", Ports: []PortMapping{{HostPort: 6379}}},
		},
	}
	snap.Processes[4].Argv = []string{"cron", "--nightly-shop-backup"}
	rows := Flatten(snap, ViewOptions{})
	tests := []struct {
		query string
		want  []string
	}{
		{"worker", []string{"[project /src/shop]", "  api", "    Worker"}},
		{"GRAND", []string{"[project /src/shop]", "  api", "    Worker", "      grandkid"}},
		{"8080", []string{"[project /src/shop]", "  api"}},
		{"80", []string{"[project /src/shop]", "  api", "    Worker", "      grandkid"}}, // prefix: 8080 and 80
		{"300", []string{"[project /src/shop]", "  vite"}},
		{"10", nil}, // digits never match pids or names
		{"nightly", []string{"[other]", "  cron"}},
		{"shop", []string{"[project /src/shop]", "  api", "    Worker", "      grandkid", "  vite", "[other]", "  cron"}}, // project name keeps the group
		{"store", []string{"[compose store]", "  docker-proxy@db"}},
		{"postgres", []string{"[compose store]", "  docker-proxy@db"}},
		{"6379", []string{"[containers]", "  ctr:redis"}},
		{"nothing", nil},
	}
	for _, tt := range tests {
		if got := render(Filter(rows, tt.query)); !slices.Equal(got, tt.want) {
			t.Errorf("Filter(%q) = %q, want %q", tt.query, got, tt.want)
		}
	}
	if got := Filter(rows, ""); len(got) != len(rows) || &got[0] != &rows[0] {
		t.Error("empty query must return the input")
	}
}

// TestFlattenHereFirst: the project devdash was run from is the first group, ahead of groups
// with newer activity; the others keep the activity order (spec "Release 1.0", TUI).
func TestFlattenHereFirst(t *testing.T) {
	snap := Snapshot{
		Projects: []Project{{ID: "/a", Name: "a", Here: true}, {ID: "/b", Name: "b"}},
		Processes: []Process{
			fp(10, 1, 0, "a1", "/a", KindOther), // a: the oldest activity
			fp(11, 1, 9, "b1", "/b", KindOther),
			fp(12, 1, 7, "proxy", "", KindContainer, 5432),
			fp(13, 1, 100, "lone", "", KindOther),
		},
		Containers: []Container{{ID: "db", Name: "db", ComposeProject: "shop", Ports: []PortMapping{{HostPort: 5432}}}},
	}
	snap.Processes[2].ContainerID = "db"
	groups := func() []string {
		var got []string
		for _, r := range Flatten(snap, ViewOptions{}) {
			if r.Depth == 0 {
				got = append(got, groupNames[r.Key.Header]+" "+r.Key.Group)
			}
		}
		return got
	}
	if got, want := groups(), []string{"project /a", "project /b", "compose shop", "other "}; !slices.Equal(got, want) {
		t.Errorf("groups %q, want %q", got, want)
	}
	snap.Projects[0].Here = false
	if got, want := groups(), []string{"project /b", "compose shop", "project /a", "other "}; !slices.Equal(got, want) {
		t.Errorf("without here: groups %q, want %q", got, want)
	}
}

// TestFilterTags: a query matches a process's tags by label or by JSON name, as a substring
// of each tag (spec "Release 1.0", TUI).
func TestFilterTags(t *testing.T) {
	const s = "/src/shop"
	orphan := fp(10, 1, 0, "api", s, KindServer, 8080)
	orphan.Tags = TagOrphaned
	gone := fp(11, 1, 1, "vite", s, KindServer, 3000)
	gone.Tags = TagOrphaned | TagCwdDeleted
	lone := fp(20, 1, 0, "watcher", "", KindOther)
	lone.Tags = TagCwdDeleted
	snap := Snapshot{
		Projects:  shop(),
		Processes: []Process{orphan, gone, fp(12, 1, 2, "worker", s, KindOther), lone},
	}
	rows := Flatten(snap, ViewOptions{})
	tests := []struct {
		query string
		want  []string
	}{
		{"orphaned", []string{"[project /src/shop]", "  api", "  vite"}},
		{"ORPHAN", []string{"[project /src/shop]", "  api", "  vite"}},
		{"cwd deleted", []string{"[project /src/shop]", "  vite", "[other]", "  watcher"}},
		{"cwd_deleted", []string{"[project /src/shop]", "  vite", "[other]", "  watcher"}},
		{"deleted", []string{"[project /src/shop]", "  vite", "[other]", "  watcher"}},
		{"orphaned, cwd", nil}, // each tag is matched on its own, not the joined list
	}
	for _, tt := range tests {
		if got := render(Filter(rows, tt.query)); !slices.Equal(got, tt.want) {
			t.Errorf("Filter(%q) = %q, want %q", tt.query, got, tt.want)
		}
	}
}

// TestMatch: Match is Filter's test of one row, without the group rule (a matching header
// keeps its whole group in Filter, but Match on a row under it looks at that row alone).
func TestMatch(t *testing.T) {
	const s = "/src/shop"
	tagged := fp(12, 1, 2, "worker", s, KindOther)
	tagged.Tags = TagCwdDeleted
	proxy := fp(30, 1, 0, "docker-proxy", "", KindContainer, 5432)
	proxy.ContainerID = "c1"
	snap := Snapshot{
		Projects:   shop(),
		Processes:  []Process{fp(10, 1, 0, "Vite", s, KindServer, 3000), fp(11, 1, 1, "zsh", s, KindShell), tagged, proxy},
		Containers: []Container{{ID: "c1", Name: "db", Image: "postgres:16", ComposeProject: "store", Ports: []PortMapping{{HostPort: 5432}}}},
	}
	all := Flatten(snap, ViewOptions{ShowAll: true})
	rows := map[string]Row{}
	for _, r := range all {
		rows[strings.TrimSpace(render([]Row{r})[0])] = r
	}
	tests := []struct {
		row   string
		query string
		want  bool
	}{
		{"[project /src/shop]", "SHOP", true},
		{"Vite", "shop", false}, // the group rule is Filter's
		{"Vite", "vI", true},
		{"Vite", "30", true},   // port prefix
		{"Vite", "10", false},  // digits never match pids or names
		{"Vite", "000", false}, // a prefix, not a substring
		{"zsh", "zsh", true},   // hidden kinds match like any row
		{"zsh", "", true},      // an empty query matches every row
		{"worker", "cwd_deleted", true},
		{"worker", "CWD DEL", true},
		{"[compose store]", "stor", true},
		{"docker-proxy@db", "postgres", true},
		{"docker-proxy@db", "5432", true},
	}
	for _, tt := range tests {
		r, ok := rows[tt.row]
		if !ok {
			t.Fatalf("no row %q in %q", tt.row, render(all))
		}
		if got := Match(r, tt.query); got != tt.want {
			t.Errorf("Match(%s, %q) = %v, want %v", tt.row, tt.query, got, tt.want)
		}
	}
}

// TestHideable: Flatten hides exactly the processes Hideable reports, unless ShowAll.
func TestHideable(t *testing.T) {
	const s = "/src/shop"
	procs := []Process{
		fp(10, 1, 0, "zsh", s, KindShell),
		fp(11, 1, 1, "nvim", s, KindEditor),
		fp(12, 1, 2, "code", s, KindEditor, 9229), // a listener is never hidden
		fp(13, 1, 3, "vite", s, KindServer),
	}
	shown := map[string]bool{}
	for _, r := range Flatten(Snapshot{Projects: shop(), Processes: procs}, ViewOptions{}) {
		if r.Process != nil {
			shown[r.Process.Name] = true
		}
	}
	for _, p := range procs {
		if p.Hideable() == shown[p.Name] {
			t.Errorf("%s: Hideable %v, shown by Flatten %v", p.Name, p.Hideable(), shown[p.Name])
		}
	}
}
