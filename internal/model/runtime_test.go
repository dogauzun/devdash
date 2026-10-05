package model

import "testing"

func TestIsContainerRuntime(t *testing.T) {
	tests := []struct {
		name string
		p    Process
		want bool
	}{
		{"docker desktop backend by name", Process{PID: 9, Name: "com.docker.backend"}, true},
		{"cut kernel name, full argv[0]", Process{PID: 9, Name: "com.docker.backe", Argv: []string{"/Applications/Docker.app/Contents/MacOS/com.docker.backend", "run"}}, true},
		{"docker-proxy", Process{PID: 9, Name: "docker-proxy", Argv: []string{"/usr/bin/docker-proxy", "-proto", "tcp"}}, true},
		{"containerd shim", Process{PID: 9, Name: "containerd-shim", Argv: []string{"/usr/bin/containerd-shim-runc-v2", "-namespace", "moby"}}, true},
		{"rootless podman", Process{PID: 9, Name: "pasta"}, true},
		{"docker desktop's mac forwarder", Process{PID: 9, Name: "com.docker.vpnk", Argv: []string{"/Applications/Docker.app/Contents/Resources/bin/com.docker.vpnkit", "--ethernet"}}, true},
		{"lima host agent", Process{PID: 9, Name: "limactl", Argv: []string{"/opt/homebrew/bin/limactl", "hostagent", "colima"}}, true},
		{"case and path", Process{PID: 9, Name: "x", Argv: []string{"/usr/local/bin/DockerD"}}, true},
		{"orbstack's port forwarder, recorded", orbHelper(9), true},
		{"orbstack's port forwarder by kernel name", Process{PID: 9, Name: "OrbStack Helper"}, true},
		{"orbstack's app", Process{PID: 9, Name: "OrbStack", Argv: []string{"/Applications/OrbStack.app/Contents/MacOS/OrbStack"}}, true},
		{"half of orbstack's forwarder name", Process{PID: 9, Name: "Helper", Argv: []string{"/usr/local/bin/Helper"}}, false},
		{"orbstack's docker cli is yours", Process{PID: 9, Name: "docker", Argv: []string{"/Applications/OrbStack.app/Contents/MacOS/xbin/docker", "compose", "up"}}, false},
		{"docker cli is yours", Process{PID: 9, Name: "docker", Argv: []string{"docker", "compose", "up"}}, false},
		{"podman cli is yours", Process{PID: 9, Name: "podman", Argv: []string{"podman", "run", "-p", "8080:80", "nginx"}}, false},
		{"a server", Process{PID: 9, Name: "node", Argv: []string{"node", "server.js"}}, false},
		{"unknown owner", Process{Name: "dockerd"}, false},
	}
	for _, tt := range tests {
		if got := IsContainerRuntime(tt.p); got != tt.want {
			t.Errorf("%s: IsContainerRuntime = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestRuntimeCLI(t *testing.T) {
	for name, want := range map[string]string{"docker-proxy": "docker", "containerd-shim-runc-v2": "docker", "pasta": "podman", "gvproxy": "", "OrbStack Helper": "docker", "OrbStack": "docker", "node": ""} {
		if got := RuntimeCLI(Process{PID: 9, Name: name}); got != want {
			t.Errorf("RuntimeCLI(%s) = %q, want %q", name, got, want)
		}
	}
}

// TestIsProxy pins the runtime processes Reconcile trusts to hold a container's published port.
func TestIsProxy(t *testing.T) {
	for name, want := range map[string]bool{
		"docker-proxy": true, "dockerd": true, "com.docker.backend": true, "com.docker.vpnkit": true,
		"vpnkit": true, "limactl": true, "gvproxy": true, "rootlesskit": true, "rootlessport": true,
		"slirp4netns": true, "pasta": true, "OrbStack Helper": true,
		"containerd": false, "conmon": false, "OrbStack": false, "containerd-shim-runc-v2": false, "node": false,
	} {
		if got := isProxy(Process{PID: 9, Name: name}); got != want {
			t.Errorf("isProxy(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestMayBeRuntime: a kernel name that could belong to a runtime process once argv[0] is known,
// so the collector reads its argv over 5000 processes (DEV-92, PR #64 review).
func TestMayBeRuntime(t *testing.T) {
	for name, want := range map[string]bool{
		"pasta.avx2":             true, // passt's pasta re-execs as pasta.avx2; argv[0] is pasta
		"dockerd":                true,
		"com.docker.vpn":         true, // a cut prefix
		"containerd-shim-runc-v": true,
		"OrbStack Helper":        true, // compared lower-case
		"-dockerd":               true, // a login-shell dash is dropped, as names() does
		"sleep":                  false,
		"node":                   false,
		"fourteen-chars":         false,
		"zsh":                    false,
	} {
		if got := MayBeRuntime(name); got != want {
			t.Errorf("MayBeRuntime(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestRuntimeLists: every list key is a lower-case basename, as names() produces them, and
// every proxy is a runtime process, so a forwarder Reconcile trusts is also refused by name
// when Docker gives no container list (spec, risk 6).
func TestRuntimeLists(t *testing.T) {
	for n := range runtimeNames {
		if n != baseName(n) {
			t.Errorf("runtimeNames[%q] never matches: names are compared as %q", n, baseName(n))
		}
	}
	for n := range proxyNames {
		if n != baseName(n) {
			t.Errorf("proxyNames[%q] never matches: names are compared as %q", n, baseName(n))
		}
		if !IsContainerRuntime(Process{PID: 9, Name: n}) {
			t.Errorf("proxy %q is not a runtime process", n)
		}
	}
}

// orbHelper is OrbStack's port forwarder as recorded on macOS 27.0.1 with OrbStack 2.2.3
// (DEV-73): p_comm and basename(argv[0]) are both "OrbStack Helper", with a space, and it runs
// as the user under launchd.
func orbHelper(pid int, ls ...Listener) Process {
	return Process{PID: pid, PPID: 1, UID: 501, User: "dogauzun", Name: "OrbStack Helper", Argv: []string{
		"/Applications/OrbStack.app/Contents/Frameworks/OrbStack Helper.app/Contents/MacOS/OrbStack Helper",
		"vmgr", "-build-id", "macho:05775f98b4bb3d75b88520220872c248", "-handoff"}, Kind: KindServer, Listeners: ls}
}
