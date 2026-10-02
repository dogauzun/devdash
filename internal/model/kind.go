package model

// Classify returns the kind of p from the basename of argv[0], the rest of argv and its
// listeners, by the first matching rule of the spec's table ("Process tree and kinds").
// It never returns KindContainer: that kind is set by Reconcile. A process with no argv
// (unreadable or blanked, and the PID 0 pseudo-process) is classified by its listeners alone.
//
// Owned by DEV-17; placeholder: KindServer when p has a listener, else KindOther.
func Classify(p Process) Kind {
	if len(p.Listeners) > 0 {
		return KindServer
	}
	return KindOther
}
