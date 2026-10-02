package docker

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dirs are one case's temporary directories. All are short so socket paths stay under macOS's
// 104-byte limit.
type dirs struct {
	home string // $HOME
	root string // stands in for "/" (rootDir)
	xdg  string // a directory to use as $XDG_RUNTIME_DIR
	cfg  string // a directory to use as $DOCKER_CONFIG
}

// shortTempDir is a temporary directory with a short path: on macOS $TMPDIR is already about
// 50 bytes long, too long for ~/.local/share/containers/podman/machine/podman.sock under it.
func shortTempDir(t *testing.T) string {
	t.Helper()
	base := ""
	if fi, err := os.Stat("/tmp"); err == nil && fi.IsDir() {
		base = "/tmp"
	}
	d, err := os.MkdirTemp(base, "dd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// mkSocket creates a listening unix socket at path, closed at the end of the test.
func mkSocket(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s (%d bytes): %v", path, len(path), err)
	}
	t.Cleanup(func() { _ = l.Close() })
}

func mkFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func contextHash(name string) string {
	h := sha256.Sum256([]byte(name))
	return hex.EncodeToString(h[:])
}

// mkContext writes the meta.json of context name under the config dir cfg.
func mkContext(t *testing.T, cfg, name, host string) {
	t.Helper()
	mkFile(t, filepath.Join(cfg, "contexts", "meta", contextHash(name), "meta.json"),
		`{"Name":"`+name+`","Metadata":{},"Endpoints":{"docker":{"Host":"`+host+`","SkipTLSVerify":false}}}`)
}

func mkCurrent(t *testing.T, cfg, name string) {
	t.Helper()
	mkFile(t, filepath.Join(cfg, "config.json"), `{"auths":{},"currentContext":"`+name+`"}`)
}

// The default socket paths, relative to the case's directories.
func desktopSock(d dirs) string  { return filepath.Join(d.home, ".docker", "run", "docker.sock") }
func orbstackSock(d dirs) string { return filepath.Join(d.home, ".orbstack", "run", "docker.sock") }
func colimaSock(d dirs) string   { return filepath.Join(d.home, ".colima", "default", "docker.sock") }
func varRunSock(d dirs) string   { return filepath.Join(d.root, "var", "run", "docker.sock") }
func xdgPodmanSock(d dirs) string {
	return filepath.Join(d.xdg, "podman", "podman.sock")
}
func machineSock(d dirs) string {
	return filepath.Join(d.home, ".local", "share", "containers", "podman", "machine", "podman.sock")
}
func dotDocker(d dirs) string { return filepath.Join(d.home, ".docker") }

func unixEP(path, source string) Endpoint {
	return Endpoint{Network: "unix", Address: path, Source: source}
}

func TestDiscover(t *testing.T) {
	tests := []struct {
		name    string
		env     func(d dirs) map[string]string
		setup   func(t *testing.T, d dirs)
		noHome  bool
		want    func(d dirs) Endpoint // nil: ok is false
		wantErr bool
	}{
		{
			name: "nothing found",
		},

		// 1. DOCKER_HOST.
		{
			name: "DOCKER_HOST unix wins over context and defaults",
			env: func(d dirs) map[string]string {
				return map[string]string{"DOCKER_HOST": "unix://" + filepath.Join(d.xdg, "custom.sock")}
			},
			setup: func(t *testing.T, d dirs) {
				mkSocket(t, filepath.Join(d.xdg, "custom.sock"))
				mkCurrent(t, dotDocker(d), "colima")
				mkContext(t, dotDocker(d), "colima", "unix://"+colimaSock(d))
				mkSocket(t, colimaSock(d))
				mkSocket(t, desktopSock(d))
			},
			want: func(d dirs) Endpoint { return unixEP(filepath.Join(d.xdg, "custom.sock"), "DOCKER_HOST") },
		},
		{
			name: "DOCKER_HOST unix is used even when the socket does not exist",
			env: func(d dirs) map[string]string {
				return map[string]string{"DOCKER_HOST": "unix:///nonexistent/docker.sock"}
			},
			setup: func(t *testing.T, d dirs) { mkSocket(t, desktopSock(d)) },
			want:  func(d dirs) Endpoint { return unixEP("/nonexistent/docker.sock", "DOCKER_HOST") },
		},
		{
			name: "DOCKER_HOST tcp",
			env: func(dirs) map[string]string {
				return map[string]string{"DOCKER_HOST": "tcp://127.0.0.1:2375"}
			},
			want: func(dirs) Endpoint { return Endpoint{Network: "tcp", Address: "127.0.0.1:2375", Source: "DOCKER_HOST"} },
		},
		{
			name: "DOCKER_HOST tcp without a port uses 2375",
			env: func(dirs) map[string]string {
				return map[string]string{"DOCKER_HOST": "tcp://docker.local"}
			},
			want: func(dirs) Endpoint {
				return Endpoint{Network: "tcp", Address: "docker.local:2375", Source: "DOCKER_HOST"}
			},
		},
		{
			name: "DOCKER_HOST tcp IPv6",
			env: func(dirs) map[string]string {
				return map[string]string{"DOCKER_HOST": "tcp://[::1]:2375/"}
			},
			want: func(dirs) Endpoint { return Endpoint{Network: "tcp", Address: "[::1]:2375", Source: "DOCKER_HOST"} },
		},
		{
			name: "DOCKER_HOST tcp with DOCKER_TLS_VERIFY is an error",
			env: func(dirs) map[string]string {
				return map[string]string{"DOCKER_HOST": "tcp://127.0.0.1:2376", "DOCKER_TLS_VERIFY": "1"}
			},
			setup:   func(t *testing.T, d dirs) { mkSocket(t, desktopSock(d)) },
			wantErr: true,
		},
		{
			name: "DOCKER_TLS_VERIFY does not affect a unix DOCKER_HOST",
			env: func(dirs) map[string]string {
				return map[string]string{"DOCKER_HOST": "unix:///run/docker.sock", "DOCKER_TLS_VERIFY": "1"}
			},
			want: func(dirs) Endpoint { return unixEP("/run/docker.sock", "DOCKER_HOST") },
		},
		{
			name:    "DOCKER_HOST ssh is an error",
			env:     func(dirs) map[string]string { return map[string]string{"DOCKER_HOST": "ssh://me@host"} },
			wantErr: true,
		},
		{
			name: "DOCKER_HOST npipe is an error",
			env: func(dirs) map[string]string {
				return map[string]string{"DOCKER_HOST": "npipe:////./pipe/docker_engine"}
			},
			wantErr: true,
		},
		{
			name:    "DOCKER_HOST without a scheme is an error",
			env:     func(dirs) map[string]string { return map[string]string{"DOCKER_HOST": "localhost:2375"} },
			wantErr: true,
		},
		{
			name:    "DOCKER_HOST unix with no path is an error",
			env:     func(dirs) map[string]string { return map[string]string{"DOCKER_HOST": "unix://"} },
			wantErr: true,
		},
		{
			name:    "DOCKER_HOST unix with a relative path is an error",
			env:     func(dirs) map[string]string { return map[string]string{"DOCKER_HOST": "unix://docker.sock"} },
			wantErr: true,
		},
		{
			name:    "DOCKER_HOST tcp with no host is an error",
			env:     func(dirs) map[string]string { return map[string]string{"DOCKER_HOST": "tcp://:2375"} },
			wantErr: true,
		},
		{
			name:    "DOCKER_HOST tcp with a path is an error",
			env:     func(dirs) map[string]string { return map[string]string{"DOCKER_HOST": "tcp://host:2375/prefix"} },
			wantErr: true,
		},

		// 2. The current docker context.
		{
			name: "current context wins over defaults",
			setup: func(t *testing.T, d dirs) {
				mkCurrent(t, dotDocker(d), "colima")
				mkContext(t, dotDocker(d), "colima", "unix://"+colimaSock(d))
				mkSocket(t, colimaSock(d))
				mkSocket(t, desktopSock(d))
			},
			want: func(d dirs) Endpoint { return unixEP(colimaSock(d), "context colima") },
		},
		{
			name: "context endpoint is used even when its socket does not exist",
			setup: func(t *testing.T, d dirs) {
				mkCurrent(t, dotDocker(d), "colima")
				mkContext(t, dotDocker(d), "colima", "unix://"+colimaSock(d))
				mkSocket(t, desktopSock(d))
			},
			want: func(d dirs) Endpoint { return unixEP(colimaSock(d), "context colima") },
		},
		{
			name: "context tcp endpoint",
			setup: func(t *testing.T, d dirs) {
				mkCurrent(t, dotDocker(d), "remote")
				mkContext(t, dotDocker(d), "remote", "tcp://10.0.0.5:2375")
			},
			want: func(dirs) Endpoint {
				return Endpoint{Network: "tcp", Address: "10.0.0.5:2375", Source: "context remote"}
			},
		},
		{
			name: "DOCKER_CONTEXT overrides currentContext",
			env:  func(dirs) map[string]string { return map[string]string{"DOCKER_CONTEXT": "orbstack"} },
			setup: func(t *testing.T, d dirs) {
				mkCurrent(t, dotDocker(d), "colima")
				mkContext(t, dotDocker(d), "colima", "unix://"+colimaSock(d))
				mkContext(t, dotDocker(d), "orbstack", "unix://"+orbstackSock(d))
			},
			want: func(d dirs) Endpoint { return unixEP(orbstackSock(d), "context orbstack") },
		},
		{
			name: "DOCKER_CONTEXT works without a config.json",
			env:  func(dirs) map[string]string { return map[string]string{"DOCKER_CONTEXT": "orbstack"} },
			setup: func(t *testing.T, d dirs) {
				mkContext(t, dotDocker(d), "orbstack", "unix://"+orbstackSock(d))
			},
			want: func(d dirs) Endpoint { return unixEP(orbstackSock(d), "context orbstack") },
		},
		{
			name: "DOCKER_CONTEXT default skips the current context",
			env:  func(dirs) map[string]string { return map[string]string{"DOCKER_CONTEXT": "default"} },
			setup: func(t *testing.T, d dirs) {
				mkCurrent(t, dotDocker(d), "colima")
				mkContext(t, dotDocker(d), "colima", "unix://"+colimaSock(d))
				mkSocket(t, desktopSock(d))
			},
			want: func(d dirs) Endpoint { return unixEP(desktopSock(d), "default") },
		},
		{
			name: "currentContext default goes to the default paths",
			setup: func(t *testing.T, d dirs) {
				mkCurrent(t, dotDocker(d), "default")
				mkSocket(t, desktopSock(d))
			},
			want: func(d dirs) Endpoint { return unixEP(desktopSock(d), "default") },
		},
		{
			name: "empty currentContext goes to the default paths",
			setup: func(t *testing.T, d dirs) {
				mkFile(t, filepath.Join(dotDocker(d), "config.json"), `{"auths":{}}`)
				mkSocket(t, desktopSock(d))
			},
			want: func(d dirs) Endpoint { return unixEP(desktopSock(d), "default") },
		},
		{
			name: "missing meta.json falls through to the default paths",
			setup: func(t *testing.T, d dirs) {
				mkCurrent(t, dotDocker(d), "gone")
				mkSocket(t, desktopSock(d))
			},
			want: func(d dirs) Endpoint { return unixEP(desktopSock(d), "default") },
		},
		{
			name: "broken config.json falls through to the default paths",
			setup: func(t *testing.T, d dirs) {
				mkFile(t, filepath.Join(dotDocker(d), "config.json"), `{"currentContext": `)
				mkContext(t, dotDocker(d), "colima", "unix://"+colimaSock(d))
				mkSocket(t, desktopSock(d))
			},
			want: func(d dirs) Endpoint { return unixEP(desktopSock(d), "default") },
		},
		{
			name: "broken meta.json falls through to the default paths",
			setup: func(t *testing.T, d dirs) {
				mkCurrent(t, dotDocker(d), "colima")
				mkFile(t, filepath.Join(dotDocker(d), "contexts", "meta", contextHash("colima"), "meta.json"), `{"Endpoints":`)
				mkSocket(t, desktopSock(d))
			},
			want: func(d dirs) Endpoint { return unixEP(desktopSock(d), "default") },
		},
		{
			name: "meta.json without a docker endpoint falls through to the default paths",
			setup: func(t *testing.T, d dirs) {
				mkCurrent(t, dotDocker(d), "k8s")
				mkFile(t, filepath.Join(dotDocker(d), "contexts", "meta", contextHash("k8s"), "meta.json"),
					`{"Name":"k8s","Endpoints":{"kubernetes":{"Host":"https://k8s:6443"}}}`)
				mkSocket(t, desktopSock(d))
			},
			want: func(d dirs) Endpoint { return unixEP(desktopSock(d), "default") },
		},
		{
			name: "context with an ssh endpoint is an error",
			setup: func(t *testing.T, d dirs) {
				mkCurrent(t, dotDocker(d), "remote")
				mkContext(t, dotDocker(d), "remote", "ssh://me@host")
				mkSocket(t, desktopSock(d))
			},
			wantErr: true,
		},
		{
			name: "context with a tcp endpoint and TLS material is an error",
			setup: func(t *testing.T, d dirs) {
				mkCurrent(t, dotDocker(d), "remote")
				mkContext(t, dotDocker(d), "remote", "tcp://10.0.0.5:2376")
				mkFile(t, filepath.Join(dotDocker(d), "contexts", "tls", contextHash("remote"), "docker", "ca.pem"), "x")
			},
			wantErr: true,
		},
		{
			name: "DOCKER_CONFIG is the config dir",
			env:  func(d dirs) map[string]string { return map[string]string{"DOCKER_CONFIG": d.cfg} },
			setup: func(t *testing.T, d dirs) {
				mkCurrent(t, d.cfg, "orbstack")
				mkContext(t, d.cfg, "orbstack", "unix://"+orbstackSock(d))
				// ~/.docker is ignored when DOCKER_CONFIG is set.
				mkCurrent(t, dotDocker(d), "colima")
				mkContext(t, dotDocker(d), "colima", "unix://"+colimaSock(d))
			},
			want: func(d dirs) Endpoint { return unixEP(orbstackSock(d), "context orbstack") },
		},
		{
			name: "DOCKER_CONFIG with no config falls through to the default paths",
			env:  func(d dirs) map[string]string { return map[string]string{"DOCKER_CONFIG": d.cfg} },
			setup: func(t *testing.T, d dirs) {
				mkCurrent(t, dotDocker(d), "colima")
				mkContext(t, dotDocker(d), "colima", "unix://"+colimaSock(d))
				mkSocket(t, desktopSock(d))
			},
			want: func(d dirs) Endpoint { return unixEP(desktopSock(d), "default") },
		},

		// 3. Default paths, in order.
		{
			name:  "Docker Desktop",
			setup: func(t *testing.T, d dirs) { mkSocket(t, desktopSock(d)) },
			want:  func(d dirs) Endpoint { return unixEP(desktopSock(d), "default") },
		},
		{
			name:  "OrbStack",
			setup: func(t *testing.T, d dirs) { mkSocket(t, orbstackSock(d)) },
			want:  func(d dirs) Endpoint { return unixEP(orbstackSock(d), "default") },
		},
		{
			name:  "Colima",
			setup: func(t *testing.T, d dirs) { mkSocket(t, colimaSock(d)) },
			want:  func(d dirs) Endpoint { return unixEP(colimaSock(d), "default") },
		},
		{
			name:  "/var/run/docker.sock",
			setup: func(t *testing.T, d dirs) { mkSocket(t, varRunSock(d)) },
			want:  func(d dirs) Endpoint { return unixEP(varRunSock(d), "default") },
		},
		{
			name:  "XDG_RUNTIME_DIR podman",
			env:   func(d dirs) map[string]string { return map[string]string{"XDG_RUNTIME_DIR": d.xdg} },
			setup: func(t *testing.T, d dirs) { mkSocket(t, xdgPodmanSock(d)) },
			want:  func(d dirs) Endpoint { return unixEP(xdgPodmanSock(d), "default") },
		},
		{
			name:  "XDG_RUNTIME_DIR unset skips its podman socket",
			setup: func(t *testing.T, d dirs) { mkSocket(t, xdgPodmanSock(d)) },
		},
		{
			name:  "podman machine",
			setup: func(t *testing.T, d dirs) { mkSocket(t, machineSock(d)) },
			want:  func(d dirs) Endpoint { return unixEP(machineSock(d), "default") },
		},
		{
			name: "first existing path wins",
			env:  func(d dirs) map[string]string { return map[string]string{"XDG_RUNTIME_DIR": d.xdg} },
			setup: func(t *testing.T, d dirs) {
				mkSocket(t, orbstackSock(d))
				mkSocket(t, colimaSock(d))
				mkSocket(t, varRunSock(d))
				mkSocket(t, xdgPodmanSock(d))
				mkSocket(t, machineSock(d))
			},
			want: func(d dirs) Endpoint { return unixEP(orbstackSock(d), "default") },
		},
		{
			name: "/var/run before podman",
			env:  func(d dirs) map[string]string { return map[string]string{"XDG_RUNTIME_DIR": d.xdg} },
			setup: func(t *testing.T, d dirs) {
				mkSocket(t, varRunSock(d))
				mkSocket(t, xdgPodmanSock(d))
				mkSocket(t, machineSock(d))
			},
			want: func(d dirs) Endpoint { return unixEP(varRunSock(d), "default") },
		},
		{
			name: "XDG podman before podman machine",
			env:  func(d dirs) map[string]string { return map[string]string{"XDG_RUNTIME_DIR": d.xdg} },
			setup: func(t *testing.T, d dirs) {
				mkSocket(t, xdgPodmanSock(d))
				mkSocket(t, machineSock(d))
			},
			want: func(d dirs) Endpoint { return unixEP(xdgPodmanSock(d), "default") },
		},
		{
			name: "regular file at a default path is not a socket",
			setup: func(t *testing.T, d dirs) {
				mkFile(t, desktopSock(d), "")
				mkSocket(t, varRunSock(d))
			},
			want: func(d dirs) Endpoint { return unixEP(varRunSock(d), "default") },
		},
		{
			name: "directory at a default path is not a socket",
			setup: func(t *testing.T, d dirs) {
				if err := os.MkdirAll(desktopSock(d), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink to a socket counts",
			setup: func(t *testing.T, d dirs) {
				mkSocket(t, varRunSock(d))
				if err := os.MkdirAll(filepath.Dir(desktopSock(d)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(varRunSock(d), desktopSock(d)); err != nil {
					t.Fatal(err)
				}
			},
			want: func(d dirs) Endpoint { return unixEP(desktopSock(d), "default") },
		},
		{
			name:   "no home: only absolute default paths",
			noHome: true,
			setup:  func(t *testing.T, d dirs) { mkSocket(t, varRunSock(d)) },
			want:   func(d dirs) Endpoint { return unixEP(varRunSock(d), "default") },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := shortTempDir(t)
			d := dirs{
				home: filepath.Join(base, "h"),
				root: filepath.Join(base, "r"),
				xdg:  filepath.Join(base, "x"),
				cfg:  filepath.Join(base, "c"),
			}
			for _, p := range []string{d.home, d.root, d.xdg, d.cfg} {
				if err := os.Mkdir(p, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			oldRoot := rootDir
			rootDir = d.root
			t.Cleanup(func() { rootDir = oldRoot })

			if tt.setup != nil {
				tt.setup(t, d)
			}
			env := map[string]string{}
			if tt.env != nil {
				env = tt.env(d)
			}
			e := Env{Getenv: func(k string) string { return env[k] }, Home: d.home}
			if tt.noHome {
				e.Home = ""
			}

			got, ok, err := Discover(e)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Discover() = %+v, %v, nil; want an error", got, ok)
				}
				if ok {
					t.Errorf("Discover() ok = true with error %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Discover() error: %v", err)
			}
			if tt.want == nil {
				if ok {
					t.Fatalf("Discover() = %+v, true; want nothing found", got)
				}
				return
			}
			if !ok {
				t.Fatalf("Discover() found nothing; want %+v", tt.want(d))
			}
			if want := tt.want(d); got != want {
				t.Errorf("Discover() = %+v; want %+v", got, want)
			}
		})
	}
}

func TestDiscoverErrorNamesTheSource(t *testing.T) {
	_, _, err := Discover(Env{Getenv: func(k string) string {
		if k == "DOCKER_HOST" {
			return "ssh://me@host"
		}
		return ""
	}})
	if err == nil {
		t.Fatal("no error")
	}
	if got := err.Error(); !strings.Contains(got, "DOCKER_HOST") || !strings.Contains(got, "ssh://me@host") {
		t.Errorf("error %q should name DOCKER_HOST and its value", got)
	}
}
