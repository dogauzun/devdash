package docker

import (
	"time"

	"github.com/dogauzun/devdash/internal/model"
)

// NewDiscoverySource returns a Source with no endpoint yet, for a dashboard started before
// Docker or Podman: each Fetch while it has none runs discover (Discover with the process's
// Env), and one that finds nothing holds off as a missing socket does, so discovery runs
// again on the first beat at or after 10 refresh ticks, with no containers and no warning in
// between. An endpoint discover returns with an error gives the docker_endpoint_invalid
// warning until the next attempt. Once discover finds an endpoint
// the Source keeps it and behaves as NewSource's for it, pinging and listing in the same
// call. It does not discover or connect until the first Fetch.
func NewDiscoverySource(discover func() (Endpoint, bool, error), tick, beat time.Duration) *Source {
	return &Source{discover: discover, timeout: requestTimeout, needPing: true, retry: newHoldoff(tick, beat), now: time.Now}
}

// InvalidEndpoint is the docker_endpoint_invalid warning for err, an endpoint that DOCKER_HOST
// or the docker context names and devdash cannot use.
func InvalidEndpoint(err error) *model.Warning {
	return &model.Warning{Code: "docker_endpoint_invalid", Count: 1, Hint: "docker: " + err.Error()}
}

// find runs discovery for a Source without an endpoint in the call begun at start, and
// reports whether it found one, which the Source now asks.
func (s *Source) find(start time.Time) bool {
	ep, ok, err := s.discover()
	switch {
	case err != nil:
		s.warn = InvalidEndpoint(err)
	case !ok:
		s.warn = nil
	default:
		s.mu.Lock()
		s.ep = ep
		s.mu.Unlock()
		s.c, s.warn = newClient(ep), nil
		return true
	}
	s.prev = nil
	s.retry.wait(start)
	return false
}
