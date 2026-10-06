# devdash

Terminal dashboard: what is running on this machine, grouped by git repository.
Spec: `docs/SPEC.md` (the authority). Work is tracked in the maintainer's private Jira project DEV;
`DEV-n` keys in code, commits and `DECISIONS.md` refer to its tickets.
Non-obvious choices get one dated line each in `DECISIONS.md`, but a PR does not edit that file: it puts its lines,
ready to paste, under `## Decisions` in the PR body, and the merger adds them to `DECISIONS.md` after the merge, in
one docs PR per batch (DEV-173).

## Commands

The `Makefile` wraps what CI runs; `make` lists the targets.

```sh
make check      # the pre-push gate: lint + vet (darwin and linux), the four cross-builds, go test -race -count=1,
                #   the run-devdash skill's drive_test.py (python3), licenses-check
make licenses   # rewrite THIRD_PARTY_LICENSES (shipped in the archives) after a dependency change
make fmt        # gofmt + goimports rewrite (golangci-lint fmt)
make build      # ./devdash for this machine
make snapshot   # goreleaser check + release --snapshot --clean into dist/ (goreleaser v2.18.2), then CI's archive
                #   check, scripts/check-archives.sh
make demo       # re-record docs/demo.gif from demo.tape (Linux as root; vhs, ttyd, ffmpeg, Chromium)
make demo-docker  # the same in a privileged Linux container (scripts/demo.Dockerfile); the way on macOS
```

Single steps: `make lint`, `make vet`, `make cross`, `make test`, `make bench`, `make docker-gate`. golangci-lint is
pinned to v2.13.2 in CI and must be built with a Go at least as new as `go.mod`'s.

## Layout

- `cmd/devdash` — flags, subcommand dispatch, exit codes (stdlib `flag`): `main.go` (`options`, `parse`, `run`),
  `json.go` (schema v1, documented in `docs/json-schema.md` and checked against it by `json_test.go`; golden file
  `testdata/snapshot.golden.json`, rewritten with `go test ./cmd/devdash -run TestJSONGolden -update`), `port.go`
  (`writePort`, the v0.1.1 lines; `writeAnswer`, the terminal answer built around them), `portjson.go` (`port N --json`),
  `free.go` (`runFree`; the package var `probe` that tests replace), `kill.go` (`runKill`: plan, confirmation, exit
  codes). The tty checks are package vars over `charmbracelet/x/term` that tests replace: `stdinTerminal` and
  `stdoutTerminal` in `kill.go`, `stdoutWidth` in `port.go`.
- `internal/model` — stdlib only, pure. Types in `model.go`; `Build` (raw sample + previous snapshot → `Snapshot`), the
  raw input types and `NeedsArgv` (the names whose argv is read over the process limit) in `build.go`; one file per
  seam: `project.go` (`Resolver`; `NewTick`, the per-tick memo the engine starts once per sample), `kind.go`
  (`Classify`), `rows.go` (`Row`, `Flatten`), `reconcile.go` (`Reconcile`), `tags.go` (`Tag`), `holders.go`
  (`Listener.TCP` and `PortMapping.TCP`, which sockets hold a TCP port; `Holders`: a TCP port's holders, as `kill N` and
  the TUI's kill result check them). `format.go` holds the text the dashboard, `port N` and `kill N` show:
  `Project.Label`, `Uptime`, `Clean`, `Location`, `Count`, `Process.PortList`, `Snapshot.ProjectNames`,
  `Snapshot.ContainerName`, `Snapshot.Here`.
- `internal/collector` — `Collector` interface in `collector.go` (no build tag, nothing OS-specific; `Result`,
  `Process`, `Listener` alias `model.Raw`, `model.Process`, `model.RawListener`; `countDenied`, the denied count both
  report); `fake.go` is the scripted test `Fake`; one implementation per OS in `collector_darwin.go` /
  `collector_linux.go` and sibling files with the same build tag; `procstat_darwin.go` / `procstat_linux.go`
  (`ProcStat`: a pid's start time and parent as the snapshot has them, or `ErrGone`; what kill's pid-reuse check
  re-reads).
- Everything lives under `internal/`; no public Go API is promised.
- `internal/engine` — the refresh loop (`Run`, `Updates`, `Refresh`), the one-shot `Snapshot` for the CLI,
  uid-to-user naming, and the kill both `kill N` and the dashboard run: `kill.go` (`NewPlan`, `Kill`), `killtext.go`
  (`KillOptions.Mode`); tested with `testing/synctest` against `collector.Fake`. `TestTagsLiveRemovedWorktree` is the
  phase 6 gate (a server in a removed worktree carries both tags).
- `internal/freeport` — the free-port search behind `free N` and `port N`'s next free line: `Find` (the first port from
  N to `Last(N)` that the snapshot does not hold and the probe can bind), `Next` (`Find` from N+1, none for 65535: the
  next free that `port N` and the dashboard show) and `Probe`, a raw-socket bind (not `net.Listen`) with its per-OS
  socket setup in `probe_linux.go` / `probe_darwin.go`. The root-only `TestFindLiveOtherUser` is the CI gate (run with
  sudo).
- `internal/tui` — the dashboard (Bubble Tea v2, Lip Gloss v2): `tui.go` (`Options`, `Model`, key routing, layout),
  `header.go` (header, footer), `style.go` (the ANSI styles), `clean.go` (`quote`, `quoteArgv`: no snapshot text reaches
  the terminal raw), and one file per feature with its own state type and `action` messages: `table.go` (a folded
  chain's label: `drawnLinks`, `chainLabel`), `rows.go` (selection by row key, filter, `unfold`: the TUI unfolds chains
  itself, not `model.Flatten`), `port.go` (digits start a port search, the holder is selected, the port line; its bind
  probe is `Options.Probe`, a fake in every test; `probeRun`, the latest free-port search, which the detail pane's next
  free shares), `detail.go`, `help.go`, `open.go` (the opener per OS in `open_darwin.go` / `open_linux.go`), `kill.go`,
  `sudo.go` (`S`, the rerun under sudo); `pager.go` is the paged window the help overlay, the kill modal and the detail
  pane share. Tested by sending `tea.Msg`s to the model and asserting on `View()`; shared fixture and helpers in
  `helpers_test.go` (`selectRow`, `rowsKeys`, `drop`, `key()` with pgup, pgdown, home and end), folded and unfolded
  chains in `fold_test.go`. Golden views at 80x24 and 120x40 in `golden_test.go`, files `testdata/*.golden`, rewritten
  with `go test ./internal/tui -run TestGolden -update`.

## Conventions

- Conventional commits (`feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`).
- Tests first. Every bug fix adds a test that reproduces it.
- No cgo; no shelling out to lsof/ss/netstat/ps on the refresh path.
- Dependencies kept short: `golang.org/x/sys`, `github.com/ebitengine/purego` (darwin), `charm.land/bubbletea/v2`, `charm.land/lipgloss/v2` (no Bubbles),
  `github.com/charmbracelet/x/term` (the tty checks in `cmd/devdash`), `github.com/charmbracelet/x/ansi` (display width,
  cut and truncate in the TUI and `port N`), `github.com/charmbracelet/colorprofile` (the `--no-color` profile; the
  styled goldens).
  No gopsutil, no Docker SDK.
- Every PR is reviewed by the `pr-reviewer` agent (`.claude/agents/pr-reviewer.md`); merge only on its
  `Verdict: APPROVE` / `Open findings: none.` with CI green.
