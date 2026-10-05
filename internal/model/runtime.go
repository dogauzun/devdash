package model

import "strings"

// IsContainerRuntime reports whether p is part of a container runtime (see runtimeNames): a
// daemon, a shim, a VM host agent or the forwarder that holds a container's published port.
// Name and the basename of argv[0] are both tried, so a rewritten argv[0] or a cut kernel name
// still match. The PID 0 "unknown owner" pseudo-process is never one.
func IsContainerRuntime(p Process) bool {
	_, ok := runtimeCLI(p)
	return ok
}

// MayBeRuntime reports whether a process with kernel name name could be a runtime process
// once its argv[0] is known: the name (as names() compares it) starts with a runtime name or
// runtimePrefix, as a runtime that re-execs under another name does (passt's pasta runs as
// pasta.avx2 with argv[0] pasta), or is a prefix of one, as a cut name is. The collector reads
// argv for such a process even over 5000 processes, so IsContainerRuntime keeps argv[0].
func MayBeRuntime(name string) bool {
	n := baseName(name)
	if strings.HasPrefix(n, runtimePrefix) || strings.HasPrefix(runtimePrefix, n) {
		return true
	}
	for r := range runtimeNames {
		if strings.HasPrefix(n, r) || strings.HasPrefix(r, n) {
			return true
		}
	}
	return false
}

// RuntimeCLI is the command that lists the containers of runtime process p: "docker",
// "podman", or "" when p serves either (or is not a runtime process).
func RuntimeCLI(p Process) string {
	cli, _ := runtimeCLI(p)
	return cli
}

func runtimeCLI(p Process) (string, bool) {
	for _, n := range names(p) {
		if cli, ok := runtimeNames[n]; ok {
			return cli, true
		}
		if strings.HasPrefix(n, runtimePrefix) {
			return "docker", true
		}
	}
	return "", false
}

// isProxy reports whether p is a runtime process that holds published ports (proxyNames).
func isProxy(p Process) bool {
	for _, n := range names(p) {
		if proxyNames[n] {
			return true
		}
	}
	return false
}

// names are p's Name (kernelName) and the lower-case basename of argv[0] (its program), the
// forms runtime lists are matched on; none for the PID 0 "unknown owner" pseudo-process.
func names(p Process) []string {
	if p.PID == 0 {
		return nil
	}
	ns := []string{kernelName(p)}
	if prog := program(p); prog != "" {
		ns = append(ns, baseName(prog))
	}
	return ns
}
