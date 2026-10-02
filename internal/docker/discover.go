package docker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Env is what discovery reads; tests fake it. Home is the user's home directory.
type Env struct {
	Getenv func(string) string
	Home   string
}

// rootDir is the filesystem root that absolute default paths (/var/run/docker.sock) are
// resolved under; tests point it at a temporary directory.
var rootDir = "/"

// defaultTCPPort is the Engine's plain-HTTP port, used when a tcp URL names none.
const defaultTCPPort = "2375"

// Discover finds the engine endpoint, in the spec's order: DOCKER_HOST when set (unix and tcp
// URLs); the endpoint of the current docker context (~/.docker/config.json currentContext,
// then that context's meta.json); then the first existing socket among the default paths. ok
// is false when nothing is found, which is not an error and shows no warning; err is a
// DOCKER_HOST or context that names an endpoint devdash cannot use.
func Discover(env Env) (ep Endpoint, ok bool, err error) {
	getenv := env.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}

	// 1. DOCKER_HOST.
	if host := getenv("DOCKER_HOST"); host != "" {
		ep, err := parseHost(host, getenv("DOCKER_TLS_VERIFY") != "")
		if err != nil {
			return Endpoint{}, false, fmt.Errorf("DOCKER_HOST=%s: %w", host, err)
		}
		ep.Source = "DOCKER_HOST"
		return ep, true, nil
	}

	// 2. The current docker context.
	cfgDir := getenv("DOCKER_CONFIG")
	if cfgDir == "" && env.Home != "" {
		cfgDir = filepath.Join(env.Home, ".docker")
	}
	if cfgDir != "" {
		name := getenv("DOCKER_CONTEXT")
		if name == "" {
			name = currentContext(cfgDir)
		}
		if name != "" && name != "default" {
			if host, found := contextHost(cfgDir, name); found {
				ep, err := parseHost(host, contextHasTLS(cfgDir, name))
				if err != nil {
					return Endpoint{}, false, fmt.Errorf("docker context %s (%s): %w", name, host, err)
				}
				ep.Source = "context " + name
				return ep, true, nil
			}
		}
	}

	// 3. The default socket paths.
	for _, p := range defaultPaths(env.Home, getenv("XDG_RUNTIME_DIR")) {
		if fi, err := os.Stat(p); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return Endpoint{Network: "unix", Address: p, Source: "default"}, true, nil
		}
	}
	return Endpoint{}, false, nil
}

// parseHost turns a unix:// or tcp:// URL into an Endpoint (without Source). tls is whether
// the endpoint would need TLS, which devdash has no client for; it only matters for tcp.
func parseHost(host string, tls bool) (Endpoint, error) {
	scheme, rest, found := strings.Cut(host, "://")
	if !found {
		return Endpoint{}, errors.New("not a unix:// or tcp:// URL")
	}
	switch scheme {
	case "unix":
		if !filepath.IsAbs(rest) {
			return Endpoint{}, errors.New("unix socket path must be absolute")
		}
		return Endpoint{Network: "unix", Address: rest}, nil
	case "tcp":
		if tls {
			return Endpoint{}, errors.New("TLS is not supported; use a unix socket or plain tcp")
		}
		u, err := url.Parse(host)
		if err != nil {
			return Endpoint{}, err
		}
		if u.Hostname() == "" {
			return Endpoint{}, errors.New("tcp URL has no host")
		}
		if u.Path != "" && u.Path != "/" {
			return Endpoint{}, errors.New("tcp URL with a path is not supported")
		}
		port := u.Port()
		if port == "" {
			port = defaultTCPPort
		}
		return Endpoint{Network: "tcp", Address: net.JoinHostPort(u.Hostname(), port)}, nil
	default:
		return Endpoint{}, fmt.Errorf("unsupported scheme %q: devdash speaks unix:// and plain tcp:// only", scheme)
	}
}

// currentContext is config.json's currentContext, or "" when the file is missing or broken.
func currentContext(cfgDir string) string {
	b, err := os.ReadFile(filepath.Join(cfgDir, "config.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		CurrentContext string `json:"currentContext"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return ""
	}
	return cfg.CurrentContext
}

// contextDir is a context's directory name in the context store: the hex sha256 of its name.
func contextDir(name string) string {
	h := sha256.Sum256([]byte(name))
	return hex.EncodeToString(h[:])
}

// contextHost is the docker endpoint's Host in context name's meta.json; found is false when
// the file is missing, broken or has no docker endpoint.
func contextHost(cfgDir, name string) (host string, found bool) {
	b, err := os.ReadFile(filepath.Join(cfgDir, "contexts", "meta", contextDir(name), "meta.json"))
	if err != nil {
		return "", false
	}
	var meta struct {
		Endpoints struct {
			Docker struct {
				Host string `json:"Host"`
			} `json:"docker"`
		} `json:"Endpoints"`
	}
	if json.Unmarshal(b, &meta) != nil || meta.Endpoints.Docker.Host == "" {
		return "", false
	}
	return meta.Endpoints.Docker.Host, true
}

// contextHasTLS reports whether the context store holds TLS material for the context's docker
// endpoint, which the docker CLI would use for a tcp host.
func contextHasTLS(cfgDir, name string) bool {
	_, err := os.Stat(filepath.Join(cfgDir, "contexts", "tls", contextDir(name), "docker"))
	return err == nil
}

// defaultPaths are the spec's default socket paths in order, leaving out those under an
// unknown home or an unset XDG_RUNTIME_DIR.
func defaultPaths(home, xdg string) []string {
	var paths []string
	if home != "" {
		paths = append(paths,
			filepath.Join(home, ".docker", "run", "docker.sock"),
			filepath.Join(home, ".orbstack", "run", "docker.sock"),
			filepath.Join(home, ".colima", "default", "docker.sock"),
		)
	}
	paths = append(paths, filepath.Join(rootDir, "var", "run", "docker.sock"))
	if xdg != "" {
		paths = append(paths, filepath.Join(xdg, "podman", "podman.sock"))
	}
	if home != "" {
		paths = append(paths, filepath.Join(home, ".local", "share", "containers", "podman", "machine", "podman.sock"))
	}
	return paths
}
