package docker

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"github.com/dogauzun/devdash/internal/model"
)

// apiPrefix pins the Engine API version: Docker 20.10 and newer and Podman's compatibility
// API both answer it.
const apiPrefix = "http://docker/v1.41"

// maxBody bounds a response body; a list of a few hundred containers is well under 1 MiB.
const maxBody = 16 << 20

// client speaks the two Engine API requests devdash needs over one endpoint.
type client struct {
	hc *http.Client
}

func newClient(ep Endpoint) *client {
	tr := &http.Transport{
		// Every request goes to ep, whatever the URL's host says. Proxy stays nil so that
		// HTTP_PROXY never captures a tcp endpoint.
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, ep.Network, ep.Address)
		},
		MaxIdleConns:       1,
		DisableCompression: true,
	}
	return &client{hc: &http.Client{Transport: tr}}
}

// get sends GET apiPrefix+path and returns the body of a 2xx answer.
func (c *client) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiPrefix+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	if len(body) > maxBody {
		return nil, fmt.Errorf("GET %s: body larger than %d bytes", path, maxBody)
	}
	return body, nil
}

// ping is GET /_ping; any 2xx answer means the engine is there.
func (c *client) ping(ctx context.Context) error {
	_, err := c.get(ctx, "/_ping")
	return err
}

// apiContainer holds the only /containers/json fields devdash reads.
type apiContainer struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
	Ports  []struct {
		IP          string `json:"IP"`
		PrivatePort uint16 `json:"PrivatePort"`
		PublicPort  uint16 `json:"PublicPort"`
		Type        string `json:"Type"`
	} `json:"Ports"`
}

// containers is GET /containers/json: running containers only (no all=1).
func (c *client) containers(ctx context.Context) ([]model.Container, error) {
	body, err := c.get(ctx, "/containers/json")
	if err != nil {
		return nil, err
	}
	return decodeContainers(body)
}

// decodeContainers turns a /containers/json body into containers sorted by name, then ID,
// each with its ports sorted by container port, protocol, host IP and host port.
func decodeContainers(body []byte) ([]model.Container, error) {
	var raw []apiContainer
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode /containers/json: %w", err)
	}
	out := make([]model.Container, 0, len(raw))
	for _, r := range raw {
		c := model.Container{
			ID:             r.ID,
			Name:           containerName(r.Names),
			Image:          r.Image,
			State:          r.State,
			ComposeProject: r.Labels["com.docker.compose.project"],
			ComposeService: r.Labels["com.docker.compose.service"],
		}
		for _, p := range r.Ports {
			m := model.PortMapping{HostPort: p.PublicPort, ContainerPort: p.PrivatePort, Proto: p.Type}
			if a, err := netip.ParseAddr(p.IP); err == nil {
				m.HostIP = a.WithZone("")
			}
			c.Ports = append(c.Ports, m)
		}
		slices.SortFunc(c.Ports, func(a, b model.PortMapping) int {
			return cmp.Or(
				cmp.Compare(a.ContainerPort, b.ContainerPort),
				strings.Compare(a.Proto, b.Proto),
				a.HostIP.Compare(b.HostIP),
				cmp.Compare(a.HostPort, b.HostPort),
			)
		})
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b model.Container) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
	})
	return out, nil
}

// containerName is the container's own name: Docker lists "/name" first but also adds
// "/other/alias" for each legacy link to it, and Podman lists "name". The first entry
// without a further "/" wins; failing that, the first entry.
func containerName(names []string) string {
	for _, n := range names {
		n = strings.TrimPrefix(n, "/")
		if !strings.Contains(n, "/") {
			return n
		}
	}
	if len(names) > 0 {
		return strings.TrimPrefix(names[0], "/")
	}
	return ""
}
