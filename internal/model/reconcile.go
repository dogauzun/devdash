package model

// Reconcile matches listeners to published container ports (spec "Docker integration",
// Reconciliation): a matched process gets ContainerID and KindContainer. It may modify procs
// in place (Build passes its own fresh slice) and returns the slice to use. Containers with no
// matching process stay only in Snapshot.Containers; Flatten turns them into rows.
//
// Owned by DEV-28 (Phase 3); placeholder: returns procs unchanged.
func Reconcile(procs []Process, containers []Container) []Process {
	return procs
}
