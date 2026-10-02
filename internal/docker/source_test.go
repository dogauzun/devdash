package docker

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dogauzun/devdash/internal/model"
)

// dockerBody is a canned Docker Engine /containers/json answer: names with a leading "/",
// links in Names, IPv4 and IPv6 bindings, an unpublished port, ports out of order.
const dockerBody = `[
 {"Id":"bbb222","Names":["/web"],"Image":"nginx:1.27","ImageID":"sha256:x","Command":"nginx -g","Created":1,
  "Ports":[{"PrivatePort":80,"Type":"tcp"},
           {"IP":"::","PrivatePort":443,"PublicPort":8443,"Type":"tcp"},
           {"IP":"0.0.0.0","PrivatePort":443,"PublicPort":8443,"Type":"tcp"}],
  "Labels":{"com.docker.compose.project":"shop","com.docker.compose.service":"web","other":"x"},
  "State":"running","Status":"Up 2 hours","HostConfig":{"NetworkMode":"default"}},
 {"Id":"aaa111","Names":["/web/db","/db"],"Image":"postgres:16","Ports":[
   {"IP":"127.0.0.1","PrivatePort":5432,"PublicPort":5432,"Type":"tcp"}],
  "Labels":{},"State":"running"}
]`

// podmanBody is a canned Podman compatibility-API answer: names without "/", IP "" for
// every-interface ports, null labels and extra fields.
const podmanBody = `[
 {"Id":"ccc333","Names":["redis"],"Image":"docker.io/library/redis:7","Ports":[
   {"IP":"","PrivatePort":6379,"PublicPort":6379,"Type":"tcp"}],
  "Labels":null,"State":"running","Pod":"","IsInfra":false,"Mounts":[],"Networks":["podman"]}
]`

var wantDocker = []model.Container{
	{
		ID: "aaa111", Name: "db", Image: "postgres:16", State: "running",
		Ports: []model.PortMapping{
			{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: 5432, ContainerPort: 5432, Proto: "tcp"},
		},
	},
	{
		ID: "bbb222", Name: "web", Image: "nginx:1.27", State: "running",
		ComposeProject: "shop", ComposeService: "web",
		Ports: []model.PortMapping{
			{ContainerPort: 80, Proto: "tcp"},
			{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: 8443, ContainerPort: 443, Proto: "tcp"},
			{HostIP: netip.MustParseAddr("::"), HostPort: 8443, ContainerPort: 443, Proto: "tcp"},
		},
	},
}

var wantPodman = []model.Container{
	{
		ID: "ccc333", Name: "redis", Image: "docker.io/library/redis:7", State: "running",
		Ports: []model.PortMapping{{HostPort: 6379, ContainerPort: 6379, Proto: "tcp"}},
	},
}

// engine is a fake Engine API on a temporary unix socket. It answers /_ping with an
// Api-Version header of version ("" sends none) and serves /_ping and /containers/json at the
// root or under any /v<version>/ prefix, except that a path under /v<tooOld>/ gets the 400
// Docker 29 sends a client below its minimum API version. down makes every request fail
// with 503; delay holds /containers/json that long (or until the client gives up).
type engine struct {
	mu       sync.Mutex
	body     string
	version  string
	tooOld   string
	down     bool
	delay    time.Duration
	requests []string // "GET /v1.41/containers/json", in arrival order
	hosts    []string
	queries  []string

	ep  Endpoint
	srv *httptest.Server
}

// newEngine starts an engine on a socket in a new temporary directory.
func newEngine(t *testing.T, body string) *engine {
	t.Helper()
	return newEngineAt(t, filepath.Join(tempDir(t), "d.sock"), body)
}

// newEngineAt starts an engine listening on the unix socket sock, advertising API 1.41.
func newEngineAt(t *testing.T, sock, body string) *engine {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	e := &engine{body: body, version: "1.41", ep: Endpoint{Network: "unix", Address: sock, Source: "default"}}
	e.srv = httptest.NewUnstartedServer(http.HandlerFunc(e.serve))
	_ = e.srv.Listener.Close()
	e.srv.Listener = ln
	e.srv.Start()
	t.Cleanup(e.srv.Close)
	return e
}

// tempDir is a short temporary directory: a unix socket path is limited to about 100 bytes,
// which t.TempDir can exceed on macOS.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// stop shuts the engine down; closing the unix listener removes the socket file.
func (e *engine) stop(t *testing.T) {
	t.Helper()
	e.srv.Close()
	if _, err := os.Stat(e.ep.Address); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("socket %s still there after stop: %v", e.ep.Address, err)
	}
}

func (e *engine) serve(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	e.requests = append(e.requests, r.Method+" "+r.URL.Path)
	e.hosts = append(e.hosts, r.Host)
	e.queries = append(e.queries, r.URL.RawQuery)
	down, delay, body, version, tooOld := e.down, e.delay, e.body, e.version, e.tooOld
	e.mu.Unlock()
	if down {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	path := r.URL.Path
	if rest, ok := strings.CutPrefix(path, "/v"); ok {
		v, p, _ := strings.Cut(rest, "/")
		if tooOld != "" && v == tooOld {
			http.Error(w, `{"message":"client version `+v+` is too old. Minimum supported API version is 1.44, please upgrade your client to a newer version"}`, http.StatusBadRequest)
			return
		}
		path = "/" + p
	}
	switch path {
	case "/_ping":
		if version != "" {
			w.Header().Set("Api-Version", version)
		}
		_, _ = w.Write([]byte("OK"))
	case "/containers/json":
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	default:
		http.NotFound(w, r)
	}
}

func (e *engine) set(f func(e *engine)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	f(e)
}

// take returns and clears the requests seen so far.
func (e *engine) take() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.requests
	e.requests = nil
	return r
}

// The requests of an engine advertising API 1.41: the ping is never versioned.
const (
	ping = "GET /_ping"
	list = "GET /v1.41/containers/json"
)

func fetch(t *testing.T, s *Source) ([]model.Container, *model.Warning) {
	t.Helper()
	return s.Fetch(context.Background())
}

func unreachable(ep Endpoint) *model.Warning {
	return &model.Warning{Code: "docker_unreachable", Count: 1, Hint: "docker: not reachable at " + ep.String()}
}

func TestFetchDecodesDocker(t *testing.T) {
	e := newEngine(t, dockerBody)
	s := NewSource(e.ep)
	got, w := fetch(t, s)
	if w != nil {
		t.Fatalf("warning = %+v, want nil", w)
	}
	if !reflect.DeepEqual(got, wantDocker) {
		t.Fatalf("containers:\n got %+v\nwant %+v", got, wantDocker)
	}
	if r := e.take(); !reflect.DeepEqual(r, []string{ping, list}) {
		t.Fatalf("requests = %q, want ping then list", r)
	}
	for i, h := range e.hosts {
		if h != "docker" {
			t.Errorf("request %d Host = %q, want docker", i, h)
		}
	}
	for i, q := range e.queries {
		if q != "" {
			t.Errorf("request %d query = %q, want none (running containers only)", i, q)
		}
	}
	if s.Endpoint() != e.ep {
		t.Errorf("Endpoint() = %+v, want %+v", s.Endpoint(), e.ep)
	}
}

func TestFetchDecodesPodman(t *testing.T) {
	e := newEngine(t, podmanBody)
	got, w := fetch(t, NewSource(e.ep))
	if w != nil {
		t.Fatalf("warning = %+v, want nil", w)
	}
	if !reflect.DeepEqual(got, wantPodman) {
		t.Fatalf("containers:\n got %+v\nwant %+v", got, wantPodman)
	}
}

func TestFetchEmptyList(t *testing.T) {
	e := newEngine(t, "[]")
	got, w := fetch(t, NewSource(e.ep))
	if w != nil || len(got) != 0 {
		t.Fatalf("Fetch = %+v, %+v; want no containers, no warning", got, w)
	}
}

func TestFetchPingsOnceThenLists(t *testing.T) {
	e := newEngine(t, dockerBody)
	s := NewSource(e.ep)
	for range 3 {
		if _, w := fetch(t, s); w != nil {
			t.Fatalf("warning = %+v", w)
		}
	}
	want := []string{ping, list, list, list}
	if r := e.take(); !reflect.DeepEqual(r, want) {
		t.Fatalf("requests = %q, want %q", r, want)
	}
}

func TestFetchNegotiatesAPIVersion(t *testing.T) {
	// Docker 29 answers 400 to any API version below 1.44.
	e := newEngine(t, dockerBody)
	e.set(func(e *engine) { e.version = "1.44"; e.tooOld = "1.41" })
	s := NewSource(e.ep)
	got, w := fetch(t, s)
	if w != nil {
		t.Fatalf("warning = %+v, want nil", w)
	}
	if !reflect.DeepEqual(got, wantDocker) {
		t.Fatalf("containers:\n got %+v\nwant %+v", got, wantDocker)
	}
	want := []string{ping, "GET /v1.44/containers/json"}
	if r := e.take(); !reflect.DeepEqual(r, want) {
		t.Fatalf("requests = %q, want %q", r, want)
	}
}

func TestFetchRenegotiatesOnEachPing(t *testing.T) {
	e := newEngine(t, dockerBody)
	s := NewSource(e.ep)
	if _, w := fetch(t, s); w != nil {
		t.Fatalf("warning = %+v", w)
	}
	e.take()

	// The engine is upgraded in place: the old version is refused, which is a failure, and
	// the retry's ping picks up the new one.
	e.set(func(e *engine) { e.version = "1.52"; e.tooOld = "1.41" })
	if _, w := fetch(t, s); !reflect.DeepEqual(w, unreachable(e.ep)) {
		t.Fatalf("warning = %+v, want %+v", w, unreachable(e.ep))
	}
	for range retryEvery - 1 {
		fetch(t, s)
	}
	e.take()
	got, w := fetch(t, s)
	if w != nil || !reflect.DeepEqual(got, wantDocker) {
		t.Fatalf("retry = %+v, %+v; want the list, no warning", got, w)
	}
	want := []string{ping, "GET /v1.52/containers/json"}
	if r := e.take(); !reflect.DeepEqual(r, want) {
		t.Fatalf("requests = %q, want %q", r, want)
	}
}

func TestFetchUnversionedWithoutAPIVersion(t *testing.T) {
	for _, version := range []string{"", "2.0", "1.", "1.4x", "v1.44", "1.44/../x"} {
		t.Run(version, func(t *testing.T) {
			e := newEngine(t, podmanBody)
			e.set(func(e *engine) { e.version = version })
			got, w := fetch(t, NewSource(e.ep))
			if w != nil || !reflect.DeepEqual(got, wantPodman) {
				t.Fatalf("Fetch = %+v, %+v; want the list, no warning", got, w)
			}
			want := []string{ping, "GET /containers/json"}
			if r := e.take(); !reflect.DeepEqual(r, want) {
				t.Fatalf("requests = %q, want %q", r, want)
			}
		})
	}
}

func TestFetchMissingSocket(t *testing.T) {
	sock := filepath.Join(tempDir(t), "d.sock")
	s := NewSource(Endpoint{Network: "unix", Address: sock})
	got, w := fetch(t, s)
	if got != nil || w != nil {
		t.Fatalf("Fetch = %+v, %+v; want no containers, no warning", got, w)
	}

	// The engine appears; calls 2..10 still make no request, the 11th pings and lists.
	e := newEngineAt(t, sock, dockerBody)
	for call := 2; call <= retryEvery; call++ {
		if got, w := fetch(t, s); got != nil || w != nil {
			t.Fatalf("call %d = %+v, %+v; want no containers, no warning", call, got, w)
		}
		if r := e.take(); len(r) != 0 {
			t.Fatalf("call %d: requests = %q, want none", call, r)
		}
	}
	got, w = fetch(t, s)
	if w != nil || !reflect.DeepEqual(got, wantDocker) {
		t.Fatalf("call 11 = %+v, %+v; want the list, no warning", got, w)
	}
	if r := e.take(); !reflect.DeepEqual(r, []string{ping, list}) {
		t.Fatalf("call 11: requests = %q, want ping then list", r)
	}
}

func TestFetchSocketRemovedClearsList(t *testing.T) {
	e := newEngine(t, dockerBody)
	s := NewSource(e.ep)
	if got, w := fetch(t, s); w != nil || !reflect.DeepEqual(got, wantDocker) {
		t.Fatalf("first call = %+v, %+v", got, w)
	}
	e.stop(t)

	got, w := fetch(t, s)
	if got != nil || w != nil {
		t.Fatalf("after removal = %+v, %+v; want no containers, no warning", got, w)
	}

	// Calls 2..10 stay off the network; the 11th finds the engine back.
	e2 := newEngineAt(t, e.ep.Address, podmanBody)
	for call := 2; call <= retryEvery; call++ {
		if got, w := fetch(t, s); got != nil || w != nil {
			t.Fatalf("call %d = %+v, %+v; want no containers, no warning", call, got, w)
		}
	}
	if r := e2.take(); len(r) != 0 {
		t.Fatalf("requests between retries = %q, want none", r)
	}
	got, w = fetch(t, s)
	if w != nil || !reflect.DeepEqual(got, wantPodman) {
		t.Fatalf("call 11 = %+v, %+v; want the new list, no warning", got, w)
	}
	if r := e2.take(); !reflect.DeepEqual(r, []string{ping, list}) {
		t.Fatalf("call 11: requests = %q, want ping then list", r)
	}
}

func TestFetchRefusedSocketIsUnreachable(t *testing.T) {
	// A socket file nobody listens on (a stopped daemon's leftover): ECONNREFUSED, not
	// ENOENT, so Docker is there but not answering.
	sock := filepath.Join(tempDir(t), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Close()
	ep := Endpoint{Network: "unix", Address: sock}
	got, w := fetch(t, NewSource(ep))
	if got != nil {
		t.Errorf("containers = %+v, want nil", got)
	}
	if !reflect.DeepEqual(w, unreachable(ep)) {
		t.Fatalf("warning = %+v, want %+v", w, unreachable(ep))
	}
}

func TestFetchUnreachableRetriesEveryTenthCall(t *testing.T) {
	e := newEngine(t, dockerBody)
	e.set(func(e *engine) { e.down = true })
	s := NewSource(e.ep)
	for call := 1; call <= 25; call++ {
		got, w := fetch(t, s)
		if got != nil {
			t.Fatalf("call %d: containers = %+v, want nil", call, got)
		}
		if !reflect.DeepEqual(w, unreachable(e.ep)) {
			t.Fatalf("call %d: warning = %+v", call, w)
		}
		r := e.take()
		var want []string
		if call%10 == 1 { // calls 1, 11, 21 retry
			want = []string{ping}
		}
		if !reflect.DeepEqual(r, want) {
			t.Fatalf("call %d: requests = %q, want %q", call, r, want)
		}
	}
}

func TestFetchFailureKeepsPreviousListAndRecovers(t *testing.T) {
	e := newEngine(t, dockerBody)
	s := NewSource(e.ep)
	if _, w := fetch(t, s); w != nil {
		t.Fatalf("warning = %+v", w)
	}
	e.take()

	e.set(func(e *engine) { e.down = true })
	got, w := fetch(t, s) // list fails: failure, warning, previous list
	if !reflect.DeepEqual(got, wantDocker) || !reflect.DeepEqual(w, unreachable(e.ep)) {
		t.Fatalf("failed call = %+v, %+v; want previous list and warning", got, w)
	}
	if r := e.take(); !reflect.DeepEqual(r, []string{list}) {
		t.Fatalf("requests = %q", r)
	}

	e.set(func(e *engine) { e.down = false; e.body = podmanBody })
	for call := 2; call <= 10; call++ { // calls 2..10 after the failure stay off the network
		got, w := fetch(t, s)
		if !reflect.DeepEqual(got, wantDocker) || !reflect.DeepEqual(w, unreachable(e.ep)) {
			t.Fatalf("call %d = %+v, %+v; want previous list and warning", call, got, w)
		}
	}
	if r := e.take(); len(r) != 0 {
		t.Fatalf("requests between retries = %q, want none", r)
	}

	got, w = fetch(t, s) // 11th call: ping again, list, warning cleared
	if w != nil || !reflect.DeepEqual(got, wantPodman) {
		t.Fatalf("recovered call = %+v, %+v; want new list, no warning", got, w)
	}
	if r := e.take(); !reflect.DeepEqual(r, []string{ping, list}) {
		t.Fatalf("requests = %q, want ping then list", r)
	}
}

func TestFetchBadJSONIsUnreachable(t *testing.T) {
	e := newEngine(t, `{"message":"not a list"`)
	got, w := fetch(t, NewSource(e.ep))
	if got != nil || !reflect.DeepEqual(w, unreachable(e.ep)) {
		t.Fatalf("Fetch = %+v, %+v; want nil list and warning", got, w)
	}
}

func TestFetchSlowKeepsPreviousList(t *testing.T) {
	e := newEngine(t, dockerBody)
	s := NewSource(e.ep)
	s.timeout = 50 * time.Millisecond
	if _, w := fetch(t, s); w != nil {
		t.Fatalf("warning = %+v", w)
	}
	e.take()

	e.set(func(e *engine) { e.delay = 5 * time.Second; e.body = podmanBody })
	start := time.Now()
	got, w := fetch(t, s)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("slow call took %v, want about the 50ms timeout", d)
	}
	if w != nil || !reflect.DeepEqual(got, wantDocker) {
		t.Fatalf("slow call = %+v, %+v; want previous list, no warning", got, w)
	}
	if r := e.take(); !reflect.DeepEqual(r, []string{list}) {
		t.Fatalf("requests = %q", r)
	}

	// A slow list is not a failure: the next call lists again without a ping, since the
	// spec pings only at start and after failures.
	e.set(func(e *engine) { e.delay = 0 })
	got, w = fetch(t, s)
	if w != nil || !reflect.DeepEqual(got, wantPodman) {
		t.Fatalf("after slow = %+v, %+v; want new list, no warning", got, w)
	}
	if r := e.take(); !reflect.DeepEqual(r, []string{list}) {
		t.Fatalf("requests = %q, want list only", r)
	}
}

func TestFetchCanceledContext(t *testing.T) {
	e := newEngine(t, dockerBody)
	s := NewSource(e.ep)
	if _, w := fetch(t, s); w != nil {
		t.Fatalf("warning = %+v", w)
	}
	e.take()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, w := s.Fetch(ctx)
	if w != nil || !reflect.DeepEqual(got, wantDocker) {
		t.Fatalf("canceled call = %+v, %+v; want previous list, no warning", got, w)
	}
	if r := e.take(); len(r) != 0 {
		t.Fatalf("requests on a canceled ctx = %q, want none", r)
	}
}

func TestFetchCancelDuringRequest(t *testing.T) {
	e := newEngine(t, dockerBody)
	s := NewSource(e.ep)
	s.timeout = 10 * time.Second
	if _, w := fetch(t, s); w != nil {
		t.Fatalf("warning = %+v", w)
	}
	e.take()

	e.set(func(e *engine) { e.delay = 10 * time.Second })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	got, w := s.Fetch(ctx)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Fetch took %v after ctx expired, want it to return with ctx", d)
	}
	if w != nil || !reflect.DeepEqual(got, wantDocker) {
		t.Fatalf("canceled call = %+v, %+v; want previous list, no warning", got, w)
	}

	// The caller giving up says nothing about the endpoint: the next call goes straight
	// to the list, with no ping and no skipped calls.
	e.set(func(e *engine) { e.delay = 0 })
	e.take()
	if _, w := fetch(t, s); w != nil {
		t.Fatalf("warning = %+v", w)
	}
	if r := e.take(); !reflect.DeepEqual(r, []string{list}) {
		t.Fatalf("requests = %q, want list only", r)
	}
}

func TestFetchTCPEndpoint(t *testing.T) {
	e := &engine{body: podmanBody, version: "1.41"}
	srv := httptest.NewServer(http.HandlerFunc(e.serve))
	t.Cleanup(srv.Close)
	ep := Endpoint{Network: "tcp", Address: srv.Listener.Addr().String()}
	got, w := fetch(t, NewSource(ep))
	if w != nil || !reflect.DeepEqual(got, wantPodman) {
		t.Fatalf("Fetch = %+v, %+v", got, w)
	}
}
