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
	"syscall"
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

// The tests' Sources are built for the default 2 s refresh tick, so a failure or a missing
// socket holds them off for testRetry; the engine calls Fetch every beat.
const (
	testTick  = 2 * time.Second
	testRetry = retryTicks * testTick // 20 s
	beat      = 5 * time.Second       // engine.DockerTick
)

// clock is a fake time for Source.now that moves only when the test moves it.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

// newSource is NewSource(ep, testTick, beat) on a fake clock.
func newSource(ep Endpoint) (*Source, *clock) { return newSourceTick(ep, testTick) }

// newSourceTick is NewSource(ep, tick, beat) on a fake clock.
func newSourceTick(ep Endpoint, tick time.Duration) (*Source, *clock) {
	c := &clock{t: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	s := NewSource(ep, tick, beat)
	s.now = c.now
	return s, c
}

func TestFetchDecodesDocker(t *testing.T) {
	e := newEngine(t, dockerBody)
	s := NewSource(e.ep, testTick, beat)
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
	got, w := fetch(t, NewSource(e.ep, testTick, beat))
	if w != nil {
		t.Fatalf("warning = %+v, want nil", w)
	}
	if !reflect.DeepEqual(got, wantPodman) {
		t.Fatalf("containers:\n got %+v\nwant %+v", got, wantPodman)
	}
}

func TestFetchEmptyList(t *testing.T) {
	e := newEngine(t, "[]")
	got, w := fetch(t, NewSource(e.ep, testTick, beat))
	if w != nil || len(got) != 0 {
		t.Fatalf("Fetch = %+v, %+v; want no containers, no warning", got, w)
	}
}

func TestFetchPingsOnceThenLists(t *testing.T) {
	e := newEngine(t, dockerBody)
	s := NewSource(e.ep, testTick, beat)
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
	s := NewSource(e.ep, testTick, beat)
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
	s, clk := newSource(e.ep)
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
	e.take()
	clk.add(testRetry)
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
			got, w := fetch(t, NewSource(e.ep, testTick, beat))
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
	s, clk := newSource(Endpoint{Network: "unix", Address: sock})
	got, w := fetch(t, s)
	if got != nil || w != nil {
		t.Fatalf("Fetch = %+v, %+v; want no containers, no warning", got, w)
	}

	// The engine appears; the beats at 5, 10 and 15 s still make no request, the one at
	// 20 s (10 ticks) pings and lists.
	e := newEngineAt(t, sock, dockerBody)
	for at := beat; at < testRetry; at += beat {
		clk.add(beat)
		if got, w := fetch(t, s); got != nil || w != nil {
			t.Fatalf("at %v: %+v, %+v; want no containers, no warning", at, got, w)
		}
		if r := e.take(); len(r) != 0 {
			t.Fatalf("at %v: requests = %q, want none", at, r)
		}
	}
	clk.add(beat)
	got, w = fetch(t, s)
	if w != nil || !reflect.DeepEqual(got, wantDocker) {
		t.Fatalf("at %v: %+v, %+v; want the list, no warning", testRetry, got, w)
	}
	if r := e.take(); !reflect.DeepEqual(r, []string{ping, list}) {
		t.Fatalf("at %v: requests = %q, want ping then list", testRetry, r)
	}
}

func TestFetchSocketRemovedClearsList(t *testing.T) {
	e := newEngine(t, dockerBody)
	s, clk := newSource(e.ep)
	if got, w := fetch(t, s); w != nil || !reflect.DeepEqual(got, wantDocker) {
		t.Fatalf("first call = %+v, %+v", got, w)
	}
	e.stop(t)

	got, w := fetch(t, s)
	if got != nil || w != nil {
		t.Fatalf("after removal = %+v, %+v; want no containers, no warning", got, w)
	}

	// Beats before 10 ticks stay off the network; the one at 10 ticks finds the engine back.
	e2 := newEngineAt(t, e.ep.Address, podmanBody)
	for at := beat; at < testRetry; at += beat {
		clk.add(beat)
		if got, w := fetch(t, s); got != nil || w != nil {
			t.Fatalf("at %v: %+v, %+v; want no containers, no warning", at, got, w)
		}
	}
	if r := e2.take(); len(r) != 0 {
		t.Fatalf("requests between retries = %q, want none", r)
	}
	clk.add(beat)
	got, w = fetch(t, s)
	if w != nil || !reflect.DeepEqual(got, wantPodman) {
		t.Fatalf("at %v: %+v, %+v; want the new list, no warning", testRetry, got, w)
	}
	if r := e2.take(); !reflect.DeepEqual(r, []string{ping, list}) {
		t.Fatalf("at %v: requests = %q, want ping then list", testRetry, r)
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
	got, w := fetch(t, NewSource(ep, testTick, beat))
	if got != nil {
		t.Errorf("containers = %+v, want nil", got)
	}
	if !reflect.DeepEqual(w, unreachable(ep)) {
		t.Fatalf("warning = %+v, want %+v", w, unreachable(ep))
	}
}

// retryCases are refresh ticks with the retry they give at the engine's 5 s beat: 10 ticks
// rounded up to a whole number of beats, so the retry is the first beat at or after 10 ticks.
var retryCases = []struct {
	tick  time.Duration
	retry time.Duration
}{
	{testTick, 20 * time.Second},
	{500 * time.Millisecond, 5 * time.Second},
	{7 * time.Second, 70 * time.Second},
	{1100 * time.Millisecond, 15 * time.Second}, // 11 s: not the 10 s beat
	{600 * time.Millisecond, 10 * time.Second},  // 6 s: not the 5 s beat
	{2100 * time.Millisecond, 25 * time.Second}, // 21 s: 1 s past the 20 s beat
}

// TestFetchRetryInterval pins the retry cadence (spec "Failure modes": "retry every 10th
// tick"): after a failure or a missing socket, no request until the first 5 s beat at or
// after 10 refresh ticks since the call that failed began, less retrySlack for a beat that
// wakes early, and one request from that point on, whatever the tick.
func TestFetchRetryInterval(t *testing.T) {
	for _, tc := range retryCases {
		tick, retry := tc.tick, tc.retry
		early := retry - retrySlack // the first instant a retry is due

		t.Run("unreachable/"+tick.String(), func(t *testing.T) {
			e := newEngine(t, dockerBody)
			e.set(func(e *engine) { e.down = true })
			s, clk := newSourceTick(e.ep, tick)
			if s.RetryAfter() != retry {
				t.Fatalf("RetryAfter = %v, want %v", s.RetryAfter(), retry)
			}
			for round := range 3 {
				if _, w := fetch(t, s); !reflect.DeepEqual(w, unreachable(e.ep)) {
					t.Fatalf("round %d: warning = %+v", round, w)
				}
				if r := e.take(); !reflect.DeepEqual(r, []string{ping}) {
					t.Fatalf("round %d: requests = %q, want a ping", round, r)
				}
				for _, d := range []time.Duration{time.Nanosecond, early / 2, early/2 - 2*time.Nanosecond} {
					clk.add(d) // up to 1 ns short of the retry, less the slack
					if _, w := fetch(t, s); !reflect.DeepEqual(w, unreachable(e.ep)) {
						t.Fatalf("round %d: warning = %+v while waiting", round, w)
					}
					if r := e.take(); len(r) != 0 {
						t.Fatalf("round %d: requests %q before %v", round, r, early)
					}
				}
				clk.add(time.Nanosecond) // retry - retrySlack after the failed call: the next round retries
			}
		})

		t.Run("missing/"+tick.String(), func(t *testing.T) {
			sock := filepath.Join(tempDir(t), "d.sock")
			s, clk := newSourceTick(Endpoint{Network: "unix", Address: sock}, tick)
			if got, w := fetch(t, s); got != nil || w != nil {
				t.Fatalf("Fetch = %+v, %+v; want no containers, no warning", got, w)
			}
			e := newEngineAt(t, sock, dockerBody)
			clk.add(early - time.Nanosecond)
			if got, w := fetch(t, s); got != nil || w != nil {
				t.Fatalf("1 ns before %v: %+v, %+v; want no containers, no warning", early, got, w)
			}
			if r := e.take(); len(r) != 0 {
				t.Fatalf("1 ns before %v: requests = %q, want none", early, r)
			}
			clk.add(time.Nanosecond)
			if got, w := fetch(t, s); w != nil || !reflect.DeepEqual(got, wantDocker) {
				t.Fatalf("at %v: %+v, %+v; want the list, no warning", early, got, w)
			}
			if r := e.take(); !reflect.DeepEqual(r, []string{ping, list}) {
				t.Fatalf("at %v: requests = %q, want ping then list", early, r)
			}
		})
	}
}

// TestFetchUnreachableRetriesEveryTenTicks: at the engine's 5 s beat and the default 2 s
// tick, an endpoint that stays down is asked at 0, 20 and 40 s, not every 10th beat (50 s).
func TestFetchUnreachableRetriesEveryTenTicks(t *testing.T) {
	e := newEngine(t, dockerBody)
	e.set(func(e *engine) { e.down = true })
	s, clk := newSource(e.ep)
	for at := time.Duration(0); at <= 45*time.Second; at += beat {
		got, w := fetch(t, s)
		if got != nil {
			t.Fatalf("at %v: containers = %+v, want nil", at, got)
		}
		if !reflect.DeepEqual(w, unreachable(e.ep)) {
			t.Fatalf("at %v: warning = %+v", at, w)
		}
		r := e.take()
		var want []string
		if at%testRetry == 0 { // 0, 20 s, 40 s retry
			want = []string{ping}
		}
		if !reflect.DeepEqual(r, want) {
			t.Fatalf("at %v: requests = %q, want %q", at, r, want)
		}
		clk.add(beat)
	}
}

// TestFetchRetryOnJitteredBeat: the engine calls Fetch on a fixed 5 s ticker grid, and
// each call starts a little after its beat by however long the goroutine took to wake.
// When the failing beat woke late and the retry beat (the first at or after 10 ticks)
// wakes early, the retry still happens on that beat, not the next one (PR #46 review: 25 s
// instead of 20 s); and no earlier beat retries, however late it wakes, even when 10 ticks
// end just after a beat (PR #46 review: --tick 1.1s retried on the 10 s beat, after about
// 9 ticks).
func TestFetchRetryOnJitteredBeat(t *testing.T) {
	lags := []struct {
		name                    string
		failing, earlier, retry time.Duration // how late each beat's Fetch starts
	}{
		{"failing late, retry early", 300 * time.Millisecond, 900 * time.Millisecond, time.Millisecond},
		{"all prompt", time.Millisecond, 2 * time.Millisecond, 0},
	}
	for _, tc := range retryCases {
		for _, l := range lags {
			t.Run(tc.tick.String()+"/"+l.name, func(t *testing.T) {
				e := newEngine(t, dockerBody)
				e.set(func(e *engine) { e.down = true })
				s, clk := newSourceTick(e.ep, tc.tick)
				grid := clk.t
				at := func(beatAt, late time.Duration) { clk.t = grid.Add(beatAt + late) }

				at(0, l.failing)
				fetch(t, s)
				if r := e.take(); !reflect.DeepEqual(r, []string{ping}) {
					t.Fatalf("failing beat: requests = %q, want a ping", r)
				}
				for b := beat; b < tc.retry; b += beat {
					at(b, l.earlier)
					fetch(t, s)
					if r := e.take(); len(r) != 0 {
						t.Fatalf("beat %v: requests = %q, want none before the %v beat", b, r, tc.retry)
					}
				}
				at(tc.retry, l.retry)
				fetch(t, s)
				if r := e.take(); !reflect.DeepEqual(r, []string{ping}) {
					t.Fatalf("beat %v: requests = %q, want the retry's ping", tc.retry, r)
				}
			})
		}
	}
}

// TestFetchRetryWithoutBeat: with no beat grid (beat 0) the holdoff is exactly 10 ticks and
// there is no slack, for a caller that does not fetch on the engine's 5 s beat.
func TestFetchRetryWithoutBeat(t *testing.T) {
	e := newEngine(t, dockerBody)
	e.set(func(e *engine) { e.down = true })
	s := NewSource(e.ep, 1100*time.Millisecond, 0)
	clk := &clock{t: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	s.now = clk.now
	if s.RetryAfter() != 11*time.Second {
		t.Fatalf("RetryAfter = %v, want 11s", s.RetryAfter())
	}
	fetch(t, s)
	e.take()
	clk.add(11*time.Second - time.Nanosecond)
	fetch(t, s)
	if r := e.take(); len(r) != 0 {
		t.Fatalf("1 ns before 11s: requests = %q, want none", r)
	}
	clk.add(time.Nanosecond)
	fetch(t, s)
	if r := e.take(); !reflect.DeepEqual(r, []string{ping}) {
		t.Fatalf("at 11s: requests = %q, want a ping", r)
	}
}

// TestFetchOneShotAfterFailure: the CLI's one-shot calls are not on the beat grid; a call
// soon after a failure (--json's second sample, 200 ms on) answers from the failed state
// without a request, and one well past the holdoff retries.
func TestFetchOneShotAfterFailure(t *testing.T) {
	e := newEngine(t, dockerBody)
	e.set(func(e *engine) { e.down = true })
	s, clk := newSource(e.ep)
	if _, w := fetch(t, s); !reflect.DeepEqual(w, unreachable(e.ep)) {
		t.Fatalf("warning = %+v", w)
	}
	e.take()
	clk.add(200 * time.Millisecond)
	if _, w := fetch(t, s); !reflect.DeepEqual(w, unreachable(e.ep)) {
		t.Fatalf("second sample: warning = %+v, want the failure's", w)
	}
	if r := e.take(); len(r) != 0 {
		t.Fatalf("second sample: requests = %q, want none", r)
	}
	clk.add(time.Minute)
	fetch(t, s)
	if r := e.take(); !reflect.DeepEqual(r, []string{ping}) {
		t.Fatalf("a minute on: requests = %q, want a ping", r)
	}
}

func TestFetchFailureKeepsPreviousListAndRecovers(t *testing.T) {
	e := newEngine(t, dockerBody)
	s, clk := newSource(e.ep)
	if _, w := fetch(t, s); w != nil {
		t.Fatalf("warning = %+v", w)
	}
	e.take()

	clk.add(beat)
	e.set(func(e *engine) { e.down = true })
	got, w := fetch(t, s) // list fails: failure, warning, previous list
	if !reflect.DeepEqual(got, wantDocker) || !reflect.DeepEqual(w, unreachable(e.ep)) {
		t.Fatalf("failed call = %+v, %+v; want previous list and warning", got, w)
	}
	if r := e.take(); !reflect.DeepEqual(r, []string{list}) {
		t.Fatalf("requests = %q", r)
	}

	e.set(func(e *engine) { e.down = false; e.body = podmanBody })
	for at := beat; at < testRetry; at += beat { // beats before 10 ticks stay off the network
		clk.add(beat)
		got, w := fetch(t, s)
		if !reflect.DeepEqual(got, wantDocker) || !reflect.DeepEqual(w, unreachable(e.ep)) {
			t.Fatalf("at %v = %+v, %+v; want previous list and warning", at, got, w)
		}
	}
	if r := e.take(); len(r) != 0 {
		t.Fatalf("requests between retries = %q, want none", r)
	}

	clk.add(beat)
	got, w = fetch(t, s) // 10 ticks after the failure: ping again, list, warning cleared
	if w != nil || !reflect.DeepEqual(got, wantPodman) {
		t.Fatalf("recovered call = %+v, %+v; want new list, no warning", got, w)
	}
	if r := e.take(); !reflect.DeepEqual(r, []string{ping, list}) {
		t.Fatalf("requests = %q, want ping then list", r)
	}
}

func TestFetchBadJSONIsUnreachable(t *testing.T) {
	e := newEngine(t, `{"message":"not a list"`)
	got, w := fetch(t, NewSource(e.ep, testTick, beat))
	if got != nil || !reflect.DeepEqual(w, unreachable(e.ep)) {
		t.Fatalf("Fetch = %+v, %+v; want nil list and warning", got, w)
	}
}

func TestFetchSlowKeepsPreviousList(t *testing.T) {
	e := newEngine(t, dockerBody)
	s, clk := newSource(e.ep)
	s.timeout = 50 * time.Millisecond
	if _, w := fetch(t, s); w != nil {
		t.Fatalf("warning = %+v", w)
	}
	e.take()

	// Spec "Failure modes": slow is handled like unreachable, so an engine that answers the
	// ping and then hangs shows the previous list with the hint, not the list alone.
	clk.add(beat)
	e.set(func(e *engine) { e.delay = 5 * time.Second; e.body = podmanBody })
	start := time.Now()
	got, w := fetch(t, s)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("slow call took %v, want about the 50ms timeout", d)
	}
	if !reflect.DeepEqual(got, wantDocker) || !reflect.DeepEqual(w, unreachable(e.ep)) {
		t.Fatalf("slow call = %+v, %+v; want previous list and warning", got, w)
	}
	if r := e.take(); !reflect.DeepEqual(r, []string{list}) {
		t.Fatalf("requests = %q", r)
	}

	e.set(func(e *engine) { e.delay = 0 })
	for at := beat; at < testRetry; at += beat {
		clk.add(beat)
		got, w := fetch(t, s)
		if !reflect.DeepEqual(got, wantDocker) || !reflect.DeepEqual(w, unreachable(e.ep)) {
			t.Fatalf("at %v = %+v, %+v; want previous list and warning", at, got, w)
		}
	}
	if r := e.take(); len(r) != 0 {
		t.Fatalf("requests between retries = %q, want none", r)
	}

	clk.add(beat)
	got, w = fetch(t, s) // 10 ticks after the slow call: ping again, list, warning cleared
	if w != nil || !reflect.DeepEqual(got, wantPodman) {
		t.Fatalf("recovered call = %+v, %+v; want new list, no warning", got, w)
	}
	if r := e.take(); !reflect.DeepEqual(r, []string{ping, list}) {
		t.Fatalf("requests = %q, want ping then list", r)
	}
}

// TestFetchSlowFirstList: a one-shot CLI call whose first list is slow says so, rather
// than looking like no Docker.
func TestFetchSlowFirstList(t *testing.T) {
	e := newEngine(t, dockerBody)
	s := NewSource(e.ep, testTick, beat)
	s.timeout = 50 * time.Millisecond
	e.set(func(e *engine) { e.delay = 5 * time.Second })
	got, w := fetch(t, s)
	if got != nil || !reflect.DeepEqual(w, unreachable(e.ep)) {
		t.Fatalf("Fetch = %+v, %+v; want nil list and warning", got, w)
	}
	if r := e.take(); !reflect.DeepEqual(r, []string{ping, list}) {
		t.Fatalf("requests = %q, want ping then list", r)
	}
}

func TestFetchCanceledContext(t *testing.T) {
	e := newEngine(t, dockerBody)
	s := NewSource(e.ep, testTick, beat)
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
	s := NewSource(e.ep, testTick, beat)
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
	got, w := fetch(t, NewSource(ep, testTick, beat))
	if w != nil || !reflect.DeepEqual(got, wantPodman) {
		t.Fatalf("Fetch = %+v, %+v", got, w)
	}
}

func denied(ep Endpoint) *model.Warning {
	return &model.Warning{Code: "docker_unreachable", Count: 1, Hint: "docker: permission denied on " + ep.Address + " (add yourself to the docker group)"}
}

// dialing replaces s's transport with one whose every dial returns err, and counts the dials.
func dialing(s *Source, err error) *int {
	n := new(int)
	s.c.hc.Transport = &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		*n++
		return nil, err
	}}
	return n
}

// TestFetchPermissionDenied: a socket the user may not open (Linux's root:docker 0660 for a
// user outside the docker group) says so instead of "not reachable", which reads as Docker
// being down. It is still a failure, with the failure holdoff: at the 5 s beat and the
// default 2 s tick it is dialled at 0, 20 and 40 s. The dial error is injected, so this runs
// as root too (DEV-76).
func TestFetchPermissionDenied(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EACCES, syscall.EPERM} {
		t.Run(errno.Error(), func(t *testing.T) {
			ep := Endpoint{Network: "unix", Address: "/var/run/docker.sock"}
			s, clk := newSource(ep)
			dials := dialing(s, &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", errno)})
			for at := time.Duration(0); at <= 45*time.Second; at += beat {
				before := *dials
				got, w := fetch(t, s)
				if got != nil || !reflect.DeepEqual(w, denied(ep)) {
					t.Fatalf("at %v = %+v, %+v; want no containers and %+v", at, got, w, denied(ep))
				}
				want := 0
				if at%testRetry == 0 { // 0, 20 s, 40 s retry
					want = 1
				}
				if n := *dials - before; n != want {
					t.Fatalf("at %v: %d dials, want %d", at, n, want)
				}
				clk.add(beat)
			}
		})
	}
}

// TestFetchPermissionDeniedTCP: a tcp endpoint has no docker group to join.
func TestFetchPermissionDeniedTCP(t *testing.T) {
	ep := Endpoint{Network: "tcp", Address: "10.0.0.5:2375"}
	s, _ := newSource(ep)
	dialing(s, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.EPERM)})
	want := &model.Warning{Code: "docker_unreachable", Count: 1, Hint: "docker: permission denied on tcp://10.0.0.5:2375"}
	if _, w := fetch(t, s); !reflect.DeepEqual(w, want) {
		t.Fatalf("warning = %+v, want %+v", w, want)
	}
}

// TestFetchUnreadableSocket: the real thing, an engine whose socket file has mode 000. Root
// opens any socket regardless of its mode, so this needs another user.
func TestFetchUnreadableSocket(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the socket's mode; TestFetchPermissionDenied covers the classification")
	}
	e := newEngine(t, dockerBody)
	if err := os.Chmod(e.ep.Address, 0); err != nil {
		t.Fatal(err)
	}
	got, w := fetch(t, NewSource(e.ep, testTick, beat))
	if got != nil || !reflect.DeepEqual(w, denied(e.ep)) {
		t.Fatalf("Fetch = %+v, %+v; want no containers and %+v", got, w, denied(e.ep))
	}
	if r := e.take(); len(r) != 0 {
		t.Errorf("requests = %q, want none", r)
	}
}
