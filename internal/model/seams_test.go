package model

import "testing"

// These pin the placeholder contracts; the owning tickets replace them.

func TestResolvePlaceholder(t *testing.T) {
	procs := []Process{proc(10, 0), {Name: "unknown"}}
	if got := NewResolver("/home/me", nil).Resolve(procs); got != nil || procs[0].ProjectID != "" || procs[1].ProjectID != "" {
		t.Errorf("projects %v, ids %q %q", got, procs[0].ProjectID, procs[1].ProjectID)
	}
}

func TestClassifyPlaceholder(t *testing.T) {
	server := proc(10, 0)
	server.Listeners = []Listener{{Proto: "tcp4", Addr: lo, Port: 80}}
	unknown := Process{Name: "unknown", Listeners: server.Listeners, Unknown: unknownOwner}
	if Classify(server) != KindServer || Classify(proc(11, 0)) != KindOther || Classify(unknown) != KindOther {
		t.Error("want server with a listener, other without, other for the PID 0 pseudo-process")
	}
}

func TestReconcilePlaceholder(t *testing.T) {
	procs := []Process{proc(10, 0)}
	got := Reconcile(procs, []Container{{ID: "abc"}})
	if len(got) != 1 || &got[0] != &procs[0] || got[0].ContainerID != "" {
		t.Errorf("got %+v", got)
	}
}

// TestRowKeysDistinct: every kind of row has a non-zero key, distinct from the others even when
// a project, a compose project and a container share a name.
func TestRowKeysDistinct(t *testing.T) {
	keys := []RowKey{
		proc(10, 0).Key(),
		{PID: 0, Listener: Listener{Proto: "tcp4", Addr: any4, Port: 22}},
		{PID: 0, Listener: Listener{Proto: "tcp4", Addr: any4, Port: 631}},
		{ContainerID: "shop"},
		{Header: GroupProject, Group: "shop"},
		{Header: GroupCompose, Group: "shop"},
		{Header: GroupContainers},
		{Header: GroupOther},
	}
	seen := map[RowKey]bool{{}: true}
	for _, k := range keys {
		if seen[k] {
			t.Errorf("key %+v is zero or repeated", k)
		}
		seen[k] = true
	}
}
