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
	if p.PID == 0 {
		return "", false
	}
	names := []string{baseName(p.Name)}
	if len(p.Argv) > 0 {
		names = append(names, baseName(p.Argv[0]))
	}
	for _, n := range names {
		if cli, ok := runtimeNames[n]; ok {
			return cli, true
		}
		if strings.HasPrefix(n, runtimePrefix) {
			return "docker", true
		}
	}
	return "", false
}
