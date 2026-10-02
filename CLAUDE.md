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

- `cmd/devdash` — flags, subcommand dispatch, exit codes (stdlib `flag`).
- `internal/collector` — `Collector` interface and types in `collector.go` (no build tag, nothing OS-specific);
  one implementation per OS in `collector_darwin.go` / `collector_linux.go` and sibling files with the same build tag.
- Everything lives under `internal/`; no public Go API is promised.
- Later packages per the spec (`engine`, `model`, `docker`, `tui`) are added when their phase starts, not before.

## Conventions

- Conventional commits (`feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`).
- Tests first. Every bug fix adds a test that reproduces it.
- No cgo; no shelling out to lsof/ss/netstat/ps on the refresh path.
- Dependencies kept short: `golang.org/x/sys`, `github.com/ebitengine/purego` (darwin), Bubble Tea/Lip Gloss/Bubbles later.
  No gopsutil, no Docker SDK.
