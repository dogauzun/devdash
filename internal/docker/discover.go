package docker

// Env is what discovery reads; tests fake it. Home is the user's home directory.
type Env struct {
	Getenv func(string) string
	Home   string
}

// Discover finds the engine endpoint, in the spec's order: DOCKER_HOST when set (unix and tcp
// URLs); the endpoint of the current docker context (~/.docker/config.json currentContext,
// then that context's meta.json); then the first existing socket among the default paths. ok
// is false when nothing is found, which is not an error and shows no warning; err is a
// DOCKER_HOST or context that names an endpoint devdash cannot use.
//
// Stub until DEV-26: finds nothing.
func Discover(env Env) (ep Endpoint, ok bool, err error) {
	return Endpoint{}, false, nil
}
