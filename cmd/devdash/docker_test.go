package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dogauzun/devdash/internal/docker"
	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
)

// Every test of this package sees no Docker unless it replaces discover itself, so no test
// depends on a Docker engine running on the machine.
func init() {
	discover = func(docker.Env) (docker.Endpoint, bool, error) { return docker.Endpoint{}, false, nil }
}

// stubDiscover replaces discover for one test; it counts calls and records the Env.
func stubDiscover(t *testing.T, ep docker.Endpoint, ok bool, err error) (calls *int, env *docker.Env) {
	t.Helper()
	saved := discover
	t.Cleanup(func() { discover = saved })
	calls, env = new(int), new(docker.Env)
	discover = func(e docker.Env) (docker.Endpoint, bool, error) {
		*calls++
		*env = e
		return ep, ok, err
	}
	return calls, env
}

func TestNoDockerSkipsDiscovery(t *testing.T) {
	calls, _ := stubDiscover(t, docker.Endpoint{Network: "unix", Address: "/run/docker.sock"}, true, nil)
	if eo := (options{NoDocker: true}).engine(fake()); eo.Docker != nil {
		t.Errorf("--no-docker: Docker = %v", eo.Docker)
	}
	var stdout, stderr bytes.Buffer
	if got := run([]string{"--no-docker", "port", "3000"}, &stdout, &stderr, fake()); got != 0 {
		t.Errorf("exit %d, stderr %q", got, stderr.String())
	}
	if *calls != 0 {
		t.Errorf("--no-docker: discover called %d times", *calls)
	}
}

func TestDockerDiscovered(t *testing.T) {
	ep := docker.Endpoint{Network: "unix", Address: "/home/u/.docker/run/docker.sock", Source: "default"}
	calls, env := stubDiscover(t, ep, true, nil)
	eo := options{}.engine(fake())
	src, ok := eo.Docker.(*docker.Source)
	if !ok || src.Endpoint() != ep {
		t.Fatalf("Docker = %#v, want a Source for %v", eo.Docker, ep)
	}
	home, _ := os.UserHomeDir()
	if *calls != 1 || env.Home != home || env.Getenv == nil {
		t.Errorf("discover called %d times with home %q (want %q), Getenv set %v", *calls, env.Home, home, env.Getenv != nil)
	}
	t.Setenv("DEVDASH_TEST_DOCKER_ENV", "x")
	if env.Getenv("DEVDASH_TEST_DOCKER_ENV") != "x" {
		t.Error("Getenv does not read the environment")
	}
}

// TestDockerRetryFollowsTick: after a failure or a missing socket the Source waits 10
// refresh ticks rounded up to the 5 s Docker beat (spec "Failure modes": "retry every 10th
// tick"), not 10 of its own fetches.
func TestDockerRetryFollowsTick(t *testing.T) {
	stubDiscover(t, docker.Endpoint{Network: "unix", Address: "/run/docker.sock"}, true, nil)
	for _, tc := range []struct {
		tick, want time.Duration
	}{
		{0, 20 * time.Second}, // the default 2 s tick
		{2 * time.Second, 20 * time.Second},
		{500 * time.Millisecond, 5 * time.Second},
		{3 * time.Second, 30 * time.Second},
		{1100 * time.Millisecond, 15 * time.Second}, // 11 s, rounded up to the 15 s Docker beat
		{600 * time.Millisecond, 10 * time.Second},  // 6 s, rounded up to the 10 s Docker beat
	} {
		src, ok := options{Tick: tc.tick}.engine(fake()).Docker.(*docker.Source)
		if !ok {
			t.Fatalf("--tick %v: Docker is not a Source", tc.tick)
		}
		if got := src.RetryAfter(); got != tc.want {
			t.Errorf("--tick %v: RetryAfter = %v, want %v", tc.tick, got, tc.want)
		}
	}
	o, err := parse([]string{"--tick", "1s"})
	if err != nil {
		t.Fatal(err)
	}
	if src := o.engine(fake()).Docker.(*docker.Source); src.RetryAfter() != 10*time.Second {
		t.Errorf("--tick 1s parsed: RetryAfter = %v, want 10s", src.RetryAfter())
	}
}

func TestDockerNotFound(t *testing.T) {
	stubDiscover(t, docker.Endpoint{}, false, nil)
	if eo := (options{}).engine(fake()); eo.Docker != nil {
		t.Errorf("nothing found: Docker = %#v, want nil", eo.Docker)
	}
}

// TestDockerEndpointInvalid: an endpoint devdash cannot use means no containers and one
// warning in every snapshot, so --json says why.
func TestDockerEndpointInvalid(t *testing.T) {
	stubDiscover(t, docker.Endpoint{}, false, errors.New(`DOCKER_HOST "ssh://box": scheme ssh is not supported`))
	eo := options{}.engine(fake())
	if eo.Docker == nil {
		t.Fatal("no source for an invalid endpoint")
	}
	cs, w := eo.Docker.Fetch(context.Background())
	if cs != nil || w == nil || w.Code != "docker_endpoint_invalid" || w.Count != 1 ||
		w.Hint != `docker: DOCKER_HOST "ssh://box": scheme ssh is not supported` {
		t.Errorf("Fetch = %v, %+v", cs, w)
	}
	c := fake() // with the timings every collector reports, so the output can be validated
	c.Steps[0].Result.Timings = model.Timing{"proctable": time.Millisecond, "argv_cwd": time.Millisecond, "listeners": time.Millisecond}
	var stdout, stderr bytes.Buffer
	if got := run([]string{"--json"}, &stdout, &stderr, c); got != 0 || !strings.Contains(stdout.String(), `"code": "docker_endpoint_invalid"`) {
		t.Errorf("--json: exit %d, stdout %s, stderr %q", got, stdout.String(), stderr.String())
	}
	validate(t, stdout.Bytes())
}

// TestTUIDockerSocket: the dashboard's detail pane names the endpoint the engine's source
// asks, from the same discovery; nothing when there is no usable endpoint (nil with
// --no-docker or an invalid endpoint, "" while nothing is found).
func TestTUIDockerSocket(t *testing.T) {
	tests := []struct {
		name     string
		o        options
		ep       docker.Endpoint
		ok       bool
		err      error
		want     string // "" means DockerSocket is nil or returns ""
		discover int    // discover calls expected
	}{
		{"default socket", options{}, docker.Endpoint{Network: "unix", Address: "/home/u/.docker/run/docker.sock", Source: "default"}, true, nil,
			"/home/u/.docker/run/docker.sock", 1},
		{"context", options{}, docker.Endpoint{Network: "unix", Address: "/home/u/.colima/default/docker.sock", Source: "context colima"}, true, nil,
			"/home/u/.colima/default/docker.sock (context colima)", 1},
		{"DOCKER_HOST tcp", options{}, docker.Endpoint{Network: "tcp", Address: "10.0.0.5:2375", Source: "DOCKER_HOST"}, true, nil,
			"tcp://10.0.0.5:2375 (DOCKER_HOST)", 1},
		{"not found", options{}, docker.Endpoint{}, false, nil, "", 1},
		{"invalid", options{}, docker.Endpoint{}, false, errors.New("DOCKER_HOST=ssh://box: unsupported scheme"), "", 1},
		{"--no-docker", options{NoDocker: true}, docker.Endpoint{Network: "unix", Address: "/run/docker.sock", Source: "default"}, true, nil, "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls, _ := stubDiscover(t, tt.ep, tt.ok, tt.err)
			tt.o.All = true
			eo := tt.o.dashboard(fake())
			e := engine.New(eo)
			to := tuiOptions(tt.o, e, eo.Docker)
			if *calls != tt.discover {
				t.Errorf("discover called %d times, want %d", *calls, tt.discover)
			}
			// Kill is left to the TUI's default, engine.Kill, which asks for no refresh: the TUI
			// asks for the kill's one refresh itself, once Kill has returned (DEV-185).
			if to.Source != e || to.Kill != nil || !to.ShowAll {
				t.Errorf("Source %v (want the engine), Kill set %v (want unset), ShowAll %v", to.Source, to.Kill != nil, to.ShowAll)
			}
			switch {
			case tt.want == "" && to.DockerSocket != nil && to.DockerSocket() != "":
				t.Errorf("DockerSocket() = %q, want nil or \"\"", to.DockerSocket())
			case tt.want != "" && to.DockerSocket == nil:
				t.Errorf("DockerSocket is nil, want %q", tt.want)
			case tt.want != "" && to.DockerSocket() != tt.want:
				t.Errorf("DockerSocket() = %q, want %q", to.DockerSocket(), tt.want)
			}
		})
	}
}

// discovered is one result of a stubbed discover.
type discovered struct {
	ep  docker.Endpoint
	ok  bool
	err error
}

// stubDiscoverSeq replaces discover for one test with one that returns results[i] on call
// i+1 and the last result after that; the func it returns counts the calls so far.
func stubDiscoverSeq(t *testing.T, results ...discovered) func() int {
	t.Helper()
	saved := discover
	t.Cleanup(func() { discover = saved })
	var mu sync.Mutex
	n := 0
	discover = func(docker.Env) (docker.Endpoint, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		r := results[min(n, len(results)-1)]
		n++
		return r.ep, r.ok, r.err
	}
	return func() int { mu.Lock(); defer mu.Unlock(); return n }
}

// TestDashboardRediscovers (DEV-75): when nothing is found at start, the dashboard's engine
// gets a Source that looks again (at its first Fetch, then on the retry cadence), so Docker
// started after devdash shows up, and the detail pane's label follows it. The label is read
// on the TUI's goroutine while the engine's Fetch sets it (run with -race). The one-shot
// commands still get no source.
func TestDashboardRediscovers(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "docker.sock") // gone again by the ping: no containers, no warning
	ep := docker.Endpoint{Network: "unix", Address: sock, Source: "default"}
	calls := stubDiscoverSeq(t, discovered{}, discovered{}, discovered{ep: ep, ok: true})

	o := options{}
	if eo := o.engine(fake()); eo.Docker != nil {
		t.Errorf("one-shot: Docker = %#v, want nil", eo.Docker)
	}
	eo := o.dashboard(fake())
	src, ok := eo.Docker.(*docker.Source)
	if !ok {
		t.Fatalf("nothing found: dashboard Docker = %#v, want a rediscovering Source", eo.Docker)
	}
	if src.RetryAfter() != 20*time.Second {
		t.Errorf("RetryAfter = %v, want 20s (10 default ticks)", src.RetryAfter())
	}
	to := tuiOptions(o, engine.New(eo), eo.Docker)
	if to.DockerSocket == nil || to.DockerSocket() != "" {
		t.Fatalf("before discovery: DockerSocket is nil (%v) or not \"\"", to.DockerSocket == nil)
	}
	if n := calls(); n != 2 {
		t.Fatalf("discover called %d times at start, want 2 (one-shot, dashboard)", n)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if cs, w := src.Fetch(context.Background()); cs != nil || w != nil {
			t.Errorf("Fetch = %v, %+v; want nothing for a socket that is gone", cs, w)
		}
	}()
	for fetching := true; fetching; {
		select {
		case <-done:
			fetching = false
		default:
			if got := to.DockerSocket(); got != "" && got != sock {
				t.Errorf("DockerSocket() = %q during Fetch", got)
			}
		}
	}
	if got := to.DockerSocket(); got != sock {
		t.Errorf("after discovery: DockerSocket() = %q, want %q", got, sock)
	}
	if n := calls(); n != 3 {
		t.Errorf("discover called %d times, want 3", n)
	}
}

// TestDashboardNoDocker: --no-docker gives the dashboard no source and no discovery.
func TestDashboardNoDocker(t *testing.T) {
	calls := stubDiscoverSeq(t, discovered{})
	if eo := (options{NoDocker: true}).dashboard(fake()); eo.Docker != nil {
		t.Errorf("--no-docker: Docker = %#v, want nil", eo.Docker)
	}
	if n := calls(); n != 0 {
		t.Errorf("--no-docker: discover called %d times", n)
	}
}

// TestDashboardRediscoveryError: an endpoint that rediscovery finds but devdash cannot use
// warns as one found at start does.
func TestDashboardRediscoveryError(t *testing.T) {
	bad := errors.New("docker context remote (tcp://box:2376): TLS is not supported; use a unix socket or plain tcp")
	stubDiscoverSeq(t, discovered{}, discovered{err: bad})
	cs, w := (options{}).dashboard(fake()).Docker.Fetch(context.Background())
	_, want := endpointError{bad}.Fetch(context.Background())
	if cs != nil || w == nil || *w != *want {
		t.Errorf("Fetch = %v, %+v; want no containers, %+v", cs, w, want)
	}
}
