// Package docker is devdash's optional Docker input (spec "Docker integration"): it finds the
// engine's socket and lists containers over the Engine API with two GET requests. It never
// changes a container. Podman's compatibility API answers the same requests.
package docker

// Endpoint is where an Engine API answers.
type Endpoint struct {
	Network string // "unix" or "tcp"
	Address string // socket path for unix, host:port for tcp
	Source  string // how it was found, for the detail pane: "DOCKER_HOST", "context <name>", or "default"
}

// String is the endpoint as a URL, unix:///path or tcp://host:port, as shown in hints.
func (e Endpoint) String() string { return e.Network + "://" + e.Address }
