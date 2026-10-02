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

// names are the lower-case basenames of p's Name and argv[0], the forms runtime lists are
// matched on; none for the PID 0 "unknown owner" pseudo-process.
func names(p Process) []string {
	if p.PID == 0 {
		return nil
	}
	ns := []string{baseName(p.Name)}
	if len(p.Argv) > 0 {
		ns = append(ns, baseName(p.Argv[0]))
	}
	return ns
}
