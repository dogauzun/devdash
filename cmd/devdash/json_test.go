package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dogauzun/devdash/internal/collector"
	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
)

var update = flag.Bool("update", false, "rewrite testdata/snapshot.golden.json")

// goldenContainers pin the encoding of containers.
var goldenContainers = []model.Container{
	{ID: "9f1c2a7b0d3e", Name: "shop-db-1", Image: "postgres:16", State: "running", ComposeProject: "shop", ComposeService: "db",
		Ports: []model.PortMapping{{HostIP: netip.IPv4Unspecified(), HostPort: 5432, ContainerPort: 5432, Proto: "tcp"}}},
	{ID: "77aa", Name: "scratch", Image: "alpine", State: "paused",
		Ports: []model.PortMapping{{ContainerPort: 80, Proto: "tcp"}}},
}

// stubDocker is a ContainerSource with a fixed answer.
type stubDocker struct {
	containers []model.Container
	warning    *model.Warning
}

func (s stubDocker) Fetch(context.Context) ([]model.Container, *model.Warning) {
	return s.containers, s.warning
}

// goldenSnapshot builds a snapshot through the engine from a scripted collector: two samples
// 200 ms apart, two repositories (one a linked worktree on a detached HEAD) under dir. With
// docker nil it fetches goldenContainers from a stub and drops the measured "docker" timing,
// so the golden file has none.
func goldenSnapshot(t *testing.T, dir string, docker engine.ContainerSource) model.Snapshot {
	t.Helper()
	write := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	shop, cart := filepath.Join(dir, "shop"), filepath.Join(dir, "shop-cart")
	write(filepath.Join(shop, ".git", "HEAD"), "ref: refs/heads/main\n")
	write(filepath.Join(shop, ".git", "worktrees", "cart", "HEAD"), "0123456789abcdef0123456789abcdef01234567\n")
	write(filepath.Join(cart, ".git"), "gitdir: "+filepath.Join(shop, ".git", "worktrees", "cart")+"\n")

	t0 := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	started := time.Date(2026, 10, 2, 9, 12, 44, 120_000_000, time.FixedZone("CEST", 2*3600))
	sample := func(at time.Duration, cpu time.Duration, late bool) collector.Step {
		procs := []collector.Process{
			{PID: 100, PPID: 1, UID: 0, StartTime: started, Name: "node", Argv: []string{"node", "node_modules/.bin/vite", "--port", "5173"},
				Cwd: shop, CPUTime: time.Second + cpu/6, RSSBytes: 187563008},
			{PID: 101, PPID: 100, UID: 54321, StartTime: started, Name: "go", Argv: []string{"go", "test", "./..."},
				Cwd: cart, CPUTime: cpu, RSSBytes: 4096},
			{PID: 1, PPID: 0, UID: 0, StartTime: started, Name: "launchd", Unknown: model.FieldArgv | model.FieldCwd | model.FieldCPU | model.FieldMem},
		}
		if late {
			procs = append(procs, collector.Process{PID: 103, PPID: 100, UID: 54321, StartTime: t0, Name: "blank", Argv: nil, Cwd: shop})
		}
		return collector.Step{Result: collector.Result{
			TakenAt:   t0.Add(at),
			Host:      model.Host{OS: "darwin", Arch: "arm64", Hostname: "mbp", UID: 501},
			Processes: procs,
			Listeners: []collector.Listener{
				{Proto: "tcp6", Addr: netip.IPv6Unspecified(), Port: 5173, PID: 100},
				{Proto: "tcp4", Addr: netip.IPv4Unspecified(), Port: 631, PID: 0},
				{Proto: "tcp6", Addr: netip.MustParseAddr("fe80::1%lo0"), Port: 8081, PID: 101}, // a scoped address keeps its zone
				{Proto: "tcp4", Addr: netip.IPv4Unspecified(), Port: 5432, PID: 0},              // root's docker-proxy
			},
			Warnings: []model.Warning{{Code: "process_fields_unreadable", Count: 1, Hint: "run with sudo"}},
			Timings:  model.Timing{"proctable": 1500 * time.Microsecond, "argv_cwd": 2250 * time.Microsecond, "listeners": 750 * time.Microsecond},
		}}
	}
	// A fixed table instead of the OS lookup, so the output does not depend on this machine's
	// accounts: uid 54321 has none, so it is named by its number, as the OS lookup would.
	users := map[int]string{0: "root", 54321: "54321"}
	golden := docker == nil
	if golden {
		// Build reconciles, so the unknown owner of 5432 (root's docker-proxy) is shop-db-1's
		// and the listener_owner_unreadable count leaves it out (DEV-77).
		docker = stubDocker{goldenContainers, nil}
	}
	o := engine.Options{
		Collector: &collector.Fake{Steps: []collector.Step{sample(0, 0, false), sample(200*time.Millisecond, 300*time.Millisecond, true)}},
		Resolver:  model.NewResolver("", nil),
		LookupUser: func(uid int) string {
			n, ok := users[uid]
			if !ok {
				t.Errorf("uid %d not in the golden user table", uid)
			}
			return n
		},
		Docker: docker,
	}
	first, err := engine.Snapshot(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	s, err := engine.SnapshotAfter(context.Background(), o, first)
	if err != nil {
		t.Fatal(err)
	}
	s.Timing["projects"] = 125 * time.Microsecond // measured by Build
	if golden {
		delete(s.Timing, "docker") // measured; the golden file pins the other timings
	}
	return s
}

func TestJSONGolden(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := writeJSON(&b, goldenSnapshot(t, dir, nil), 3*time.Millisecond+456*time.Microsecond); err != nil {
		t.Fatal(err)
	}
	got := []byte(strings.ReplaceAll(b.String(), dir, "/TMP"))
	validate(t, got)

	golden := filepath.Join("testdata", "snapshot.golden.json")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output differs from %s (go test ./cmd/devdash -run TestJSONGolden -update):\n%s", golden, got)
	}
}

// TestJSONDocker checks output with a Docker source against docs/json-schema.md: its
// containers, its warning and the "docker" timing, whose value is measured, so only its
// presence is checked.
func TestJSONDocker(t *testing.T) {
	src := stubDocker{goldenContainers, &model.Warning{Code: "docker_unreachable", Count: 1, Hint: "docker: no answer"}}
	var b bytes.Buffer
	if err := writeJSON(&b, goldenSnapshot(t, t.TempDir(), src), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	validate(t, b.Bytes())

	var s struct {
		Containers []struct{ ID string }
		Warnings   []struct{ Code string }
		TimingMS   map[string]float64 `json:"timing_ms"`
	}
	if err := json.Unmarshal(b.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Containers) != 2 || s.Containers[0].ID != "9f1c2a7b0d3e" {
		t.Errorf("containers %+v", s.Containers)
	}
	if _, ok := s.TimingMS["docker"]; !ok {
		t.Errorf("timing_ms %v has no docker", s.TimingMS)
	}
	if !slices.ContainsFunc(s.Warnings, func(w struct{ Code string }) bool { return w.Code == "docker_unreachable" }) {
		t.Errorf("warnings %+v have no docker_unreachable", s.Warnings)
	}
}

// TestJSONLive checks a real snapshot of this machine against docs/json-schema.md.
func TestJSONLive(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--json"}, &stdout, &stderr, collector.New()); code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	validate(t, stdout.Bytes())

	var s struct {
		Processes []struct {
			PID        int
			CPUPercent *float64 `json:"cpu_percent"`
		}
	}
	if err := json.Unmarshal(stdout.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Processes {
		if p.PID == os.Getpid() && p.CPUPercent == nil {
			t.Error("this test's own process has no cpu_percent")
		}
	}
}

// field is one row of a docs/json-schema.md table.
type field struct {
	typ      string
	required bool
}

// schemaDoc reads the field tables of docs/json-schema.md: section heading → field → row.
func schemaDoc(t *testing.T) map[string]map[string]field {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "docs", "json-schema.md"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	doc := map[string]map[string]field{}
	section := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if h, ok := strings.CutPrefix(line, "## "); ok {
			section = h
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 5 || !strings.HasPrefix(strings.TrimSpace(cells[1]), "`") {
			continue
		}
		name := strings.Trim(strings.TrimSpace(cells[1]), "`")
		if doc[section] == nil {
			doc[section] = map[string]field{}
		}
		doc[section][name] = field{strings.TrimSpace(cells[2]), strings.TrimSpace(cells[3]) == "always"}
	}
	if doc["snapshot"] == nil || doc["process"] == nil || doc["port_answer"] == nil {
		t.Fatalf("no field tables found: %v", doc)
	}
	return doc
}

// validate checks that out is one JSON document whose fields are exactly the documented ones,
// with the documented types, and whose warning codes are all documented.
func validate(t *testing.T, out []byte) {
	t.Helper()
	validateAs(t, out, "snapshot")
}

// validateAs is validate for a document whose top-level object is the documented type root
// ("snapshot" for --json, "port_answer" for port N --json).
func validateAs(t *testing.T, out []byte, root string) {
	t.Helper()
	var v any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	doc := schemaDoc(t)
	var check func(v any, typ, path string)
	check = func(v any, typ, path string) {
		typ, nullable := strings.CutSuffix(typ, " or null")
		if v == nil {
			if !nullable {
				t.Errorf("%s: null, want %s", path, typ)
			}
			return
		}
		ok := true
		switch elem, isArray := strings.CutSuffix(typ, " array"); {
		case isArray:
			var a []any
			if a, ok = v.([]any); ok {
				for i, e := range a {
					check(e, elem, fmt.Sprintf("%s[%d]", path, i))
				}
			}
		case doc[typ] != nil:
			var m map[string]any
			if m, ok = v.(map[string]any); ok {
				for k, e := range m {
					f, documented := doc[typ][k]
					if !documented {
						t.Errorf("%s.%s: not in docs/json-schema.md (%s)", path, k, typ)
						continue
					}
					check(e, f.typ, path+"."+k)
				}
				for k, f := range doc[typ] {
					if _, present := m[k]; f.required && !present {
						t.Errorf("%s.%s: missing", path, k)
					}
				}
			}
		case typ == "string":
			_, ok = v.(string)
		case typ == "boolean":
			_, ok = v.(bool)
		case typ == "number", typ == "integer":
			var n float64
			n, ok = v.(float64)
			ok = ok && (typ == "number" || n == math.Trunc(n))
		default:
			t.Errorf("%s: docs/json-schema.md names unknown type %q", path, typ)
		}
		if !ok {
			t.Errorf("%s: %v is not %s", path, v, typ)
		}
	}
	check(v, root, "$")
	top, _ := v.(map[string]any)
	ws, _ := top["warnings"].([]any)
	for i, w := range ws {
		m, _ := w.(map[string]any)
		if code, _ := m["code"].(string); doc["Warning codes"][code] == (field{}) {
			t.Errorf("$.warnings[%d].code: %q not in docs/json-schema.md (Warning codes)", i, code)
		}
	}
}
