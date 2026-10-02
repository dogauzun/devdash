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
		{"case and path", Process{PID: 9, Name: "x", Argv: []string{"/usr/local/bin/DockerD"}}, true},
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
