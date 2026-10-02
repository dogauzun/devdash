package docker

import (
	"context"
	"errors"
	"io/fs"
	"runtime"
	"sync"
	"time"

	"github.com/dogauzun/devdash/internal/model"
)

// requestTimeout bounds each Engine API request (spec "Docker integration").
const requestTimeout = 500 * time.Millisecond

// Source lists the containers at one endpoint with the spec's failure policy, and satisfies
// engine.ContainerSource. Fetch is called by one goroutine at a time; Endpoint may be called
// alongside it.
type Source struct {
	ep      Endpoint
	c       *client
	timeout time.Duration // per request; requestTimeout except in tests

	// A NewDiscoverySource has a zero ep and a nil c until discover finds an endpoint. mu
	// guards that one write of ep against Endpoint; Fetch, the only writer, reads ep unlocked.
	discover func() (Endpoint, bool, error) // nil for NewSource
	mu       sync.Mutex

	prev     []model.Container // last good list, returned when a call cannot get a new one
	warn     *model.Warning    // docker_unreachable while the endpoint is failing (or find's docker_endpoint_invalid), else nil
	needPing bool              // ping before the next list: at start, after each failure or missing socket
	retry    holdoff           // after a failure or a missing socket, no request until it is due
	now      func() time.Time  // time.Now except in tests
}

// NewSource returns a Source for ep that, after a failure or a missing socket, makes no
// request until 10 refresh ticks of length tick (the --tick interval) have passed, retrying
// on the first beat at or after that: beat is the interval Fetch is called at
// (engine.DockerTick), or 0 for no grid. It does not connect.
func NewSource(ep Endpoint, tick, beat time.Duration) *Source {
	return &Source{ep: ep, c: newClient(ep), timeout: requestTimeout, needPing: true, retry: newHoldoff(tick, beat), now: time.Now}
}

// RetryAfter is how long a failure or a missing socket keeps this Source off the network:
// 10 refresh ticks, rounded up to a whole number of beats.
func (s *Source) RetryAfter() time.Duration { return s.retry.after }

// Endpoint is the endpoint this Source asks, for the detail pane's footer; the zero Endpoint
// while a NewDiscoverySource has found none.
func (s *Source) Endpoint() Endpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ep
}

// Fetch returns the containers to use now and a warning when Docker is present but not
// answering (nil otherwise). It pings once at start and after each failure; after a failure
// or a missing socket it makes no request until RetryAfter has passed since the start of the
// call that failed, so the retry is the first call at or after that time; with a beat, a
// call up to retrySlack (1 s) early counts, so the engine's beat that is nominally
// RetryAfter later retries even when it wakes sooner after its beat than the failing call
// did. ctx bounds the whole call.
//
// A unix socket that does not exist (ENOENT on the dial, at the ping or the list) is no
// Docker at all: it clears the list, returns no warning, and calls until the retry return
// nothing without a request. An endpoint that exists but does not answer (refused dial,
// ping or list slower than 500 ms, non-2xx, bad JSON, a dial the socket's mode denies) sets
// the docker_unreachable warning, whose hint says which (see hint), and calls until the
// retry return the previous list without a request (spec "Failure modes": "unreachable or
// slow"). A ctx that ends first is neither: the call returns the previous list and changes
// nothing. A good list replaces the previous one and clears the warning.
func (s *Source) Fetch(ctx context.Context) ([]model.Container, *model.Warning) {
	start := s.now()
	if !s.retry.due(start) {
		return s.prev, s.warn
	}
	if ctx.Err() != nil {
		return s.prev, s.warn
	}
	if s.c == nil && !s.find(start) {
		return s.prev, s.warn
	}
	if s.needPing {
		rctx, cancel := context.WithTimeout(ctx, s.timeout)
		err := s.c.ping(rctx)
		cancel()
		switch {
		case err == nil:
			s.needPing = false
		case ctx.Err() != nil:
			return s.prev, s.warn
		case s.missing(err):
			return s.absent(start)
		default:
			return s.fail(start, err)
		}
	}
	rctx, cancel := context.WithTimeout(ctx, s.timeout)
	list, err := s.c.containers(rctx)
	cancel()
	switch {
	case err == nil:
		s.prev, s.warn = list, nil
	case ctx.Err() != nil:
		// The caller gave up; that says nothing about the endpoint.
	case s.missing(err):
		return s.absent(start)
	default:
		return s.fail(start, err)
	}
	return s.prev, s.warn
}

// missing reports whether err says the unix socket file does not exist, which means
// Docker is not installed or not running here rather than failing.
func (s *Source) missing(err error) bool {
	return s.ep.Network == "unix" && errors.Is(err, fs.ErrNotExist)
}

// absent records a socket that does not exist, in the call begun at start: no containers
// and no warning, with the failure holdoff so a missing socket is looked for again only once
// 10 refresh ticks have passed.
func (s *Source) absent(start time.Time) ([]model.Container, *model.Warning) {
	s.prev, s.warn = nil, nil
	s.needPing = true
	s.retry.wait(start)
	return nil, nil
}

// fail records an endpoint that did not answer, with err, in the call begun at start.
func (s *Source) fail(start time.Time, err error) ([]model.Container, *model.Warning) {
	s.needPing = true
	s.retry.wait(start)
	s.warn = &model.Warning{Code: "docker_unreachable", Count: 1, Hint: s.hint(err)}
	return s.prev, s.warn
}

// hint is the footer line for a failure with err. An endpoint that refuses this user access
// (EACCES or EPERM on the dial: on Linux /var/run/docker.sock is root:docker 0660) is not
// "not reachable", which reads as Docker being down.
func (s *Source) hint(err error) string {
	switch {
	case errors.Is(err, fs.ErrPermission) && s.ep.Network == "unix":
		return deniedHint(runtime.GOOS, s.ep.Address)
	case errors.Is(err, fs.ErrPermission):
		return "docker: permission denied on " + s.ep.String()
	}
	return "docker: not reachable at " + s.ep.String()
}

// deniedHint is the hint for a unix socket at path that refuses this user on goos. Joining the
// docker group is Linux's remedy; on macOS the Docker Desktop and OrbStack sockets live in the
// user's home and belong to the user, so a denied one is most likely another user's (DEV-93).
func deniedHint(goos, path string) string {
	if goos == "darwin" {
		return "docker: permission denied on " + path + " (owned by another user?)"
	}
	return "docker: permission denied on " + path + " (add yourself to the docker group)"
}
