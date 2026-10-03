# devdash

Terminal dashboard: what is running on this machine, grouped by git repository.
Spec: `docs/SPEC.md` (the authority). Work is tracked in the maintainer's private Jira project DEV;
`DEV-n` keys in code, commits and `DECISIONS.md` refer to its tickets.
Non-obvious choices go in `DECISIONS.md`, one dated line each.

## Commands

The `Makefile` wraps what CI runs; `make` lists the targets.

```sh
make check      # the pre-push gate: lint + vet (darwin and linux), the four cross-builds, go test -race -count=1,
                #   licenses-check
make licenses   # rewrite THIRD_PARTY_LICENSES (shipped in the archives) after a dependency change
make fmt        # gofmt + goimports rewrite (golangci-lint fmt)
make build      # ./devdash for this machine
make snapshot   # goreleaser check + release --snapshot --clean into dist/ (goreleaser v2.18.2)
make demo       # re-record docs/demo.gif from demo.tape (Linux as root; vhs, ttyd, ffmpeg, Chromium)
```

Single steps: `make lint`, `make vet`, `make cross`, `make test`, `make bench`, `make docker-gate`. golangci-lint is
pinned to v2.13.2 in CI and must be built with a Go at least as new as `go.mod`'s.

## Layout

- `cmd/devdash` — flags, subcommand dispatch, exit codes (stdlib `flag`): `main.go` (`options`, `parse`, `run`),
  `json.go` (schema v1, documented in `docs/json-schema.md` and checked against it by `json_test.go`; golden file
  `testdata/snapshot.golden.json`, rewritten with `go test ./cmd/devdash -run TestJSONGolden -update`), `port.go`
  (`writePort`, the v0.1.1 lines; `writeAnswer`, the terminal answer built around them), `portjson.go` (`port N --json`),
  `free.go` (`runFree`; the package var `probe` that tests replace), `kill.go` (`runKill`: plan, confirmation, exit
  codes), `tty_darwin.go` / `tty_linux.go` (the termios and winsize ioctls that `isTerminal` and `stdoutWidth` use).
- `internal/model` — stdlib only, pure. Types in `model.go`; `Build` (raw sample + previous snapshot → `Snapshot`)
  and the raw input types in `build.go`; one file per seam: `project.go` (`Resolver`), `kind.go` (`Classify`),
  `rows.go` (`Row`, `Flatten`), `reconcile.go` (`Reconcile`), `tags.go` (`Tag`). `format.go` holds the text both the
  dashboard and `port N` show: `Project.Label`, `Uptime`, `Clean`.
- `internal/collector` — `Collector` interface in `collector.go` (no build tag, nothing OS-specific; `Result`, `Process`,
  `Listener` alias `model.Raw`, `model.Process`, `model.RawListener`); `fake.go` is the scripted test `Fake`;
  one implementation per OS in `collector_darwin.go` / `collector_linux.go` and sibling files with the same build tag.
- Everything lives under `internal/`; no public Go API is promised.
- `internal/engine` — the refresh loop (`Run`, `Updates`, `Refresh`), the one-shot `Snapshot` for the CLI, and
  uid-to-user naming; tested with `testing/synctest` against `collector.Fake`. `TestTagsLiveRemovedWorktree` is the
  phase 6 gate (a server in a removed worktree carries both tags).
- `internal/freeport` — the free-port search behind `free N` and `port N`'s next free line: `Find` (the first port from
  N to `Last(N)` that the snapshot does not hold and the probe can bind) and `Probe`, a raw-socket bind (not
  `net.Listen`) with its per-OS socket setup in `probe_linux.go` / `probe_darwin.go`. The root-only
  `TestFindLiveOtherUser` is the CI gate (run with sudo).
- `internal/tui` — the dashboard (Bubble Tea v2, Lip Gloss v2): `tui.go` (`Options`, `Model`, key routing, layout),
  `header.go` (header, footer), and one file per feature with its own state type and `action` messages: `table.go`,
  `rows.go` (selection by row key, filter), `detail.go`, `help.go`, `open.go`, `kill.go`. Tested by sending `tea.Msg`s
  to the model and asserting on `View()`; shared fixture in `helpers_test.go`. Golden views at 80x24 and 120x40 in
  `golden_test.go`, files `testdata/*.golden`, rewritten with `go test ./internal/tui -run TestGolden -update`.

## Conventions

- Conventional commits (`feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`).
- Tests first. Every bug fix adds a test that reproduces it.
- No cgo; no shelling out to lsof/ss/netstat/ps on the refresh path.
- Dependencies kept short: `golang.org/x/sys`, `github.com/ebitengine/purego` (darwin), `charm.land/bubbletea/v2`, `charm.land/lipgloss/v2` (no Bubbles).
  No gopsutil, no Docker SDK.
- Every PR is reviewed by the `pr-reviewer` agent (`.claude/agents/pr-reviewer.md`); merge only on its
  `Verdict: APPROVE` / `Open findings: none.` with CI green.
