package docker

import (
	"context"

	"github.com/dogauzun/devdash/internal/model"
)

// Source lists the containers at one endpoint with the spec's failure policy, and satisfies
// engine.ContainerSource. Fetch is called by one goroutine at a time.
type Source struct {
	ep Endpoint
}

// NewSource returns a Source for ep. It does not connect.
func NewSource(ep Endpoint) *Source { return &Source{ep: ep} }

// Endpoint is the endpoint this Source asks, for the detail pane's footer.
func (s *Source) Endpoint() Endpoint { return s.ep }

// Fetch returns the containers to use now and a warning when Docker is present but not
// answering (nil otherwise). It pings once at start and after each failure, retries an
// unreachable endpoint only every 10th call, and keeps the previous list when a request takes
// longer than 500 ms; ctx bounds the whole call.
//
// Stub until DEV-27: no containers, no warning.
func (s *Source) Fetch(ctx context.Context) ([]model.Container, *model.Warning) {
	return nil, nil
}
