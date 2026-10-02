package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dogauzun/devdash/internal/docker"
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
// refresh ticks (spec "Failure modes": "retry every 10th tick"), not 10 of its own fetches.
func TestDockerRetryFollowsTick(t *testing.T) {
	stubDiscover(t, docker.Endpoint{Network: "unix", Address: "/run/docker.sock"}, true, nil)
	for _, tc := range []struct {
		tick, want time.Duration
	}{
		{0, 20 * time.Second}, // the default 2 s tick
		{2 * time.Second, 20 * time.Second},
		{500 * time.Millisecond, 5 * time.Second},
		{3 * time.Second, 30 * time.Second},
	} {
		src, ok := options{Tick: tc.tick}.engine(fake()).Docker.(*docker.Source)
		if !ok {
			t.Fatalf("--tick %v: Docker is not a Source", tc.tick)
		}
		if got := src.RetryAfter(); got != tc.want {
			t.Errorf("--tick %v: RetryAfter = %v, want %v", tc.tick, got, tc.want)
		}
	}
	o, err := parse([]string{"--tick", "1s"}, io.Discard)
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
