package docker

import (
	"context"
	"errors"
	"io/fs"
	"time"

	"github.com/dogauzun/devdash/internal/model"
)

const (
	// requestTimeout bounds each Engine API request (spec "Docker integration").
	requestTimeout = 500 * time.Millisecond
	// retryEvery is the cadence of retries after a failure or a missing socket: every 10th
	// Fetch call.
	retryEvery = 10
)

// Source lists the containers at one endpoint with the spec's failure policy, and satisfies
// engine.ContainerSource. Fetch is called by one goroutine at a time.
type Source struct {
	ep      Endpoint
	c       *client
	timeout time.Duration // per request; requestTimeout except in tests

	prev     []model.Container // last good list, returned when a call cannot get a new one
	warn     *model.Warning    // docker_unreachable while the endpoint is failing, else nil
	needPing bool              // ping before the next list: at start, after each failure or missing socket
	skip     int               // Fetch calls left to answer from prev without the network
}

// NewSource returns a Source for ep. It does not connect.
func NewSource(ep Endpoint) *Source {
	return &Source{ep: ep, c: newClient(ep), timeout: requestTimeout, needPing: true}
}

// Endpoint is the endpoint this Source asks, for the detail pane's footer.
func (s *Source) Endpoint() Endpoint { return s.ep }

// Fetch returns the containers to use now and a warning when Docker is present but not
// answering (nil otherwise). It pings once at start and after each failure, and retries
// only every 10th call after a failure or a missing socket; ctx bounds the whole call.
//
// A unix socket that does not exist (ENOENT on the dial, at the ping or the list) is no
// Docker at all: it clears the list, returns no warning, and makes the next 9 calls return
// nothing without a request. An endpoint that exists but does not answer (refused dial,
// ping or list slower than 500 ms, non-2xx, bad JSON) sets the docker_unreachable warning
// and makes the next 9 calls return the previous list without a request (spec "Failure
// modes": "unreachable or slow"). A ctx that ends first is neither: the call returns the previous list and changes nothing. A good list
// replaces the previous one and clears the warning.
func (s *Source) Fetch(ctx context.Context) ([]model.Container, *model.Warning) {
	if s.skip > 0 {
		s.skip--
		return s.prev, s.warn
	}
	if ctx.Err() != nil {
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
			return s.absent()
		default:
			return s.fail()
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
		return s.absent()
	default:
		return s.fail()
	}
	return s.prev, s.warn
}

// missing reports whether err says the unix socket file does not exist, which means
// Docker is not installed or not running here rather than failing.
func (s *Source) missing(err error) bool {
	return s.ep.Network == "unix" && errors.Is(err, fs.ErrNotExist)
}

// absent records a socket that does not exist: no containers and no warning, with the
// failure cadence so a missing socket is looked for again only every 10th call.
func (s *Source) absent() ([]model.Container, *model.Warning) {
	s.prev, s.warn = nil, nil
	s.needPing = true
	s.skip = retryEvery - 1
	return nil, nil
}

// fail records an endpoint that did not answer.
func (s *Source) fail() ([]model.Container, *model.Warning) {
	s.needPing = true
	s.skip = retryEvery - 1
	s.warn = &model.Warning{Code: "docker_unreachable", Count: 1, Hint: "docker: not reachable at " + s.ep.String()}
	return s.prev, s.warn
}
