package docker

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/dogauzun/devdash/internal/model"
)

// countedDiscover wraps discover and counts its calls.
type countedDiscover struct {
	mu    sync.Mutex
	calls int
	f     func() (Endpoint, bool, error)
}

func (c *countedDiscover) discover() (Endpoint, bool, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.f()
}

func (c *countedDiscover) take() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.calls
	c.calls = 0
	return n
}

// newDiscoverySource is NewDiscoverySource(d, testTick, beat) on a fake clock.
func newDiscoverySource(d *countedDiscover) (*Source, *clock) {
	c := &clock{t: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	s := NewDiscoverySource(d.discover, testTick, beat)
	s.now = c.now
	return s, c
}

// TestDiscoverySourceFindsLateSocket (DEV-75): with no socket at any default path when
// devdash starts, discovery runs again on the same cadence as a missing socket at a known
// endpoint (the first beat at or after 10 ticks), silently, and the containers appear once
// the engine's socket exists; between attempts nothing is looked for.
func TestDiscoverySourceFindsLateSocket(t *testing.T) {
	d := dirs{home: shortTempDir(t), root: shortTempDir(t)}
	oldRoot := rootDir
	rootDir = d.root
	t.Cleanup(func() { rootDir = oldRoot })
	env := Env{Getenv: func(string) string { return "" }, Home: d.home}
	disc := &countedDiscover{f: func() (Endpoint, bool, error) { return Discover(env) }}
	s, clk := newDiscoverySource(disc)

	var e *engine
	for at := time.Duration(0); at <= 2*testRetry; at += beat {
		if at == testRetry+beat { // Docker Desktop starts after the 20 s attempt found nothing
			if err := os.MkdirAll(filepath.Dir(desktopSock(d)), 0o755); err != nil {
				t.Fatal(err)
			}
			e = newEngineAt(t, desktopSock(d), dockerBody)
		}
		got, w := fetch(t, s)
		switch {
		case at < 2*testRetry:
			if got != nil || w != nil {
				t.Fatalf("at %v: Fetch = %+v, %+v; want no containers, no warning", at, got, w)
			}
			if ep := s.Endpoint(); ep != (Endpoint{}) {
				t.Fatalf("at %v: Endpoint() = %+v before anything was found", at, ep)
			}
		default:
			if w != nil || !reflect.DeepEqual(got, wantDocker) {
				t.Fatalf("at %v: Fetch = %+v, %+v; want the list, no warning", at, got, w)
			}
			if r := e.take(); !reflect.DeepEqual(r, []string{ping, list}) {
				t.Fatalf("at %v: requests = %q, want ping then list", at, r)
			}
			if ep, want := s.Endpoint(), unixEP(desktopSock(d), "default"); ep != want {
				t.Fatalf("at %v: Endpoint() = %+v, want %+v", at, ep, want)
			}
		}
		want := 0
		if at%testRetry == 0 { // 0, 20 s and 40 s look again
			want = 1
		}
		if n := disc.take(); n != want {
			t.Fatalf("at %v: discover called %d times, want %d", at, n, want)
		}
		if e != nil && at < 2*testRetry {
			if r := e.take(); len(r) != 0 {
				t.Fatalf("at %v: requests = %q before the retry", at, r)
			}
		}
		clk.add(beat)
	}

	// Found, the Source keeps the endpoint: the next beat lists without discovery or a ping.
	if got, w := fetch(t, s); w != nil || !reflect.DeepEqual(got, wantDocker) {
		t.Fatalf("next beat: Fetch = %+v, %+v", got, w)
	}
	if n, r := disc.take(), e.take(); n != 0 || !reflect.DeepEqual(r, []string{list}) {
		t.Fatalf("next beat: discover called %d times, requests %q; want 0 and a list", n, r)
	}
}

// TestDiscoverySourceInvalidEndpoint: an endpoint discovery finds but devdash cannot use (a
// docker context switched to a TLS host mid-run) shows the same docker_endpoint_invalid
// warning as at start until the retry, and discovery goes on.
func TestDiscoverySourceInvalidEndpoint(t *testing.T) {
	e := newEngine(t, podmanBody)
	bad := errors.New("docker context remote (tcp://box:2376): TLS is not supported; use a unix socket or plain tcp")
	var mu sync.Mutex
	usable := false
	disc := &countedDiscover{f: func() (Endpoint, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if !usable {
			return Endpoint{}, false, bad
		}
		return e.ep, true, nil
	}}
	s, clk := newDiscoverySource(disc)

	wantW := &model.Warning{Code: "docker_endpoint_invalid", Count: 1, Hint: "docker: " + bad.Error()}
	if got, w := fetch(t, s); got != nil || !reflect.DeepEqual(w, wantW) {
		t.Fatalf("Fetch = %+v, %+v; want no containers, %+v", got, w, wantW)
	}
	if !reflect.DeepEqual(InvalidEndpoint(bad), wantW) {
		t.Fatalf("InvalidEndpoint = %+v, want %+v", InvalidEndpoint(bad), wantW)
	}
	clk.add(beat)
	if got, w := fetch(t, s); got != nil || !reflect.DeepEqual(w, wantW) {
		t.Fatalf("5 s: Fetch = %+v, %+v; want the warning kept", got, w)
	}
	if n := disc.take(); n != 1 {
		t.Fatalf("discover called %d times, want 1", n)
	}

	mu.Lock()
	usable = true
	mu.Unlock()
	clk.add(testRetry - beat)
	if got, w := fetch(t, s); w != nil || !reflect.DeepEqual(got, wantPodman) {
		t.Fatalf("at the retry: Fetch = %+v, %+v; want the list, no warning", got, w)
	}
	if s.Endpoint() != e.ep {
		t.Fatalf("Endpoint() = %+v, want %+v", s.Endpoint(), e.ep)
	}
}

// TestDiscoverySourceLosesSocket: once found, a socket that goes away is a missing socket at
// that endpoint, as for NewSource: no containers, no warning, and the same path is asked
// again 10 ticks later.
func TestDiscoverySourceLosesSocket(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "d.sock")
	e := newEngineAt(t, sock, dockerBody)
	disc := &countedDiscover{f: func() (Endpoint, bool, error) { return e.ep, true, nil }}
	s, clk := newDiscoverySource(disc)
	if got, w := fetch(t, s); w != nil || !reflect.DeepEqual(got, wantDocker) {
		t.Fatalf("Fetch = %+v, %+v", got, w)
	}
	e.stop(t)
	clk.add(beat)
	if got, w := fetch(t, s); got != nil || w != nil {
		t.Fatalf("after stop: Fetch = %+v, %+v; want nothing", got, w)
	}
	if s.Endpoint() != e.ep {
		t.Fatalf("Endpoint() = %+v, want %+v kept", s.Endpoint(), e.ep)
	}
	e2 := newEngineAt(t, sock, podmanBody)
	clk.add(testRetry)
	if got, w := fetch(t, s); w != nil || !reflect.DeepEqual(got, wantPodman) {
		t.Fatalf("at the retry: Fetch = %+v, %+v", got, w)
	}
	if n, r := disc.take(), e2.take(); n != 1 || !reflect.DeepEqual(r, []string{ping, list}) {
		t.Fatalf("discover called %d times (want 1, at start), requests %q", n, r)
	}
}

// TestDiscoverySourceEndpointConcurrent: the detail pane reads Endpoint on the TUI's
// goroutine while Fetch, on the engine's, sets it on discovery (run with -race).
func TestDiscoverySourceEndpointConcurrent(t *testing.T) {
	e := newEngine(t, dockerBody)
	var mu sync.Mutex
	found := false
	disc := &countedDiscover{f: func() (Endpoint, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		return e.ep, found, nil
	}}
	s, clk := newDiscoverySource(disc)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				if ep := s.Endpoint(); ep != (Endpoint{}) && ep != e.ep {
					t.Errorf("Endpoint() = %+v", ep)
				}
			}
		}
	}()
	fetch(t, s)
	mu.Lock()
	found = true
	mu.Unlock()
	clk.add(testRetry)
	got, _ := fetch(t, s)
	close(stop)
	<-done
	if !reflect.DeepEqual(got, wantDocker) || s.Endpoint() != e.ep {
		t.Fatalf("Fetch = %+v, Endpoint() = %+v", got, s.Endpoint())
	}
}
