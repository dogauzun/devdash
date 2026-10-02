# devdash

Terminal dashboard: what is running on this machine, grouped by git repository.
Spec: `docs/SPEC.md` (the authority). Work is tracked in Jira project DEV.
Non-obvious choices go in `DECISIONS.md`, one dated line each.

## Commands

```sh
CGO_ENABLED=0 go build ./...
go vet ./...
go test -race ./...
golangci-lint run
```

Check the other OS too: `GOOS=linux CGO_ENABLED=0 go build ./...` and `GOOS=linux go vet ./...` (or `GOOS=darwin`).

## Layout

- `cmd/devdash` — flags, subcommand dispatch, exit codes (stdlib `flag`): `main.go` (`options`, `parse`, `run`),
  `json.go` (schema v1, documented in `docs/json-schema.md` and checked against it by `json_test.go`; golden file
  `testdata/snapshot.golden.json`, rewritten with `go test ./cmd/devdash -run TestJSONGolden -update`), `port.go`.
- `internal/model` — stdlib only, pure. Types in `model.go`; `Build` (raw sample + previous snapshot → `Snapshot`)
  and the raw input types in `build.go`; one file per seam: `project.go` (`Resolver`), `kind.go` (`Classify`),
  `rows.go` (`Row`, `Flatten`), `reconcile.go` (`Reconcile`).
- `internal/collector` — `Collector` interface in `collector.go` (no build tag, nothing OS-specific; `Result`, `Process`,
  `Listener` alias `model.Raw`, `model.Process`, `model.RawListener`); `fake.go` is the scripted test `Fake`;
  one implementation per OS in `collector_darwin.go` / `collector_linux.go` and sibling files with the same build tag.
- Everything lives under `internal/`; no public Go API is promised.
- `internal/engine` — the refresh loop (`Run`, `Updates`, `Refresh`), the one-shot `Snapshot` for the CLI, and
  uid-to-user naming; tested with `testing/synctest` against `collector.Fake`.
- `internal/tui` — the dashboard (Bubble Tea v2, Lip Gloss v2): `tui.go` (`Options`, `Model`, key routing, layout),
  `header.go` (header, footer), and one file per feature with its own state type and `action` messages: `table.go`,
  `rows.go` (selection by row key, filter), `detail.go`, `help.go`, `open.go`, `kill.go`. Tested by sending `tea.Msg`s
  to the model and asserting on `View()`; shared fixture in `helpers_test.go`.
- Later packages per the spec (`docker`) are added when their phase starts, not before.

## Conventions

- Conventional commits (`feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`).
- Tests first. Every bug fix adds a test that reproduces it.
- No cgo; no shelling out to lsof/ss/netstat/ps on the refresh path.
- Dependencies kept short: `golang.org/x/sys`, `github.com/ebitengine/purego` (darwin), `charm.land/bubbletea/v2`, `charm.land/lipgloss/v2` (no Bubbles).
  No gopsutil, no Docker SDK.
