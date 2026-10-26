---
name: run-devdash
description: Run the devdash dashboard and drive it from an agent session - send keys, read the screen as text, and look at a known process tree. Use to see a TUI change working in the real app, not only in tests.
---

# Run devdash

The dashboard takes over the terminal, so an agent drives it through `drive.py` in this
directory: it runs the binary in a pty, types each step's keys and prints the screen as text.
It needs only Python 3. tmux is not required (it is not installed on the maintainer's Mac, and
the built-in `screen` 4.00.03 captures blank frames from Bubble Tea v2).

## Run (for agents)

From the repository root:

```bash
make build
python3 .claude/skills/run-devdash/drive.py --lines 12 '<down><down>' '<enter>' '?'
```

The screen is printed once at start and once after every step:

```
=== start
  mbp · 1 s ago · 6 projects · 11 listeners · 0 containers
  NAME                                KIND      PORTS           PID   UP   CPU   MEM USER     COMMAND
» ▾ chain @ main (here) · 8 processes · 0 ports
    ▾ bash  orphaned                  shell                   53942   4s   0.0    2M dogauzun /bin/bash ./link.sh 4
```

- `» ` marks the selected row. It is found by reverse video, which is why the default command
  is `./devdash --no-color`.
- A step is literal keys plus names in angle brackets: `<up> <down> <left> <right> <enter>
  <esc> <tab> <backspace> <pgup> <pgdown> <home> <end> <ctrl-c> <ctrl-u>`, and `<wait:3>` to
  let a refresh tick pass (`--tick`, 2 s by default). `'/vite'` types a filter, `'5173'` a
  port search.
- `--size 140x40` is the default. The golden views are 80x24 and 120x40; below 100 columns
  cpu, mem and user are dropped, below 90 the command.
- `--cwd DIR` is where the dashboard runs; the repository there is its `(here)` project and is
  listed first.
- `--cmd "./devdash --no-color --no-docker"` changes the command or its flags; `--lines N`
  prints only the top of each screen.
- The driver quits the dashboard itself (`esc`, `q`, then SIGTERM).

## A known process tree

The dashboard shows this machine's real processes. For a tree you control, `chain.sh` starts
nested scripts ending in two sleeps, in a throwaway git repository:

```bash
dir=$(mktemp -d)/chain
.claude/skills/run-devdash/chain.sh start "$dir" 4      # prints the pids it started
python3 .claude/skills/run-devdash/drive.py --cwd "$dir" --lines 9 '<down><down>' '<right>' 'x'
.claude/skills/run-devdash/chain.sh stop "$dir"         # stops exactly those pids
```

The first script is re-parented to launchd or init, so its row carries the `orphaned` tag
and keeps a row of its own; the scripts below it fold into one row.

## Keys

| Key | Action |
|---|---|
| `↑` `↓` `j` `k` | move the selection |
| `←` `→` `h` `l` | collapse or expand a group or node; `→` unfolds a chain, `←` on its first row refolds |
| `enter` | open or close the detail pane (`esc` closes it too) |
| `/` | filter; `esc` clears |
| `0`-`9` | port search |
| `x` | kill modal: `p` process, `t` tree, `f` force, `enter` confirm, `esc` cancel |
| `o` | open `http://localhost:<port>` in the browser |
| `a` `d` `s` `r` | shells and editors, container rows, sort, refresh |
| `S` | rerun under sudo, after `y` |
| `?` | help |
| `q` `ctrl-c` | quit |

`internal/tui/help.go` is the authority.

## Safe use on a real machine

- `x` opens the kill plan; read it and send `<esc>`. Send `<enter>` (and `Y` for a process
  outside every project) only for a process this session started, such as the chain above.
- Never send `y` after `S`: it replaces the dashboard with `sudo`, which waits for a password.
- `o` opens the user's browser; avoid it unless the task is about `o`.
- The `other` group starts collapsed and holds every process outside a project.

## Host quirks

What a QA pass on the maintainer's Mac and in a Linux container runs into.

### Fixture processes

- macOS kills a copy of `/bin/sleep` under another name at start (exit 137: a platform binary
  outside its path). For a process with a chosen name, build `scripts/demoproc` and copy it
  under that name; it sleeps, and `DEMO_CHILD="sleep 600"` gives it a child.
- `bun` is not installed on the maintainer's Mac; fake its argv the way `scripts/demo.sh` fakes
  its tools, with demoproc copied as `bun` into a directory on `PATH`.
- Fixture processes need a living parent: a subshell that starts them and then `wait`s. Started
  with a plain `&` from a script that exits, they are re-parented to launchd, get the
  `orphaned` tag, and a tagged process is never inside a chain, so `nodemon › server.js` and
  `air › api` do not fold.
- For a hidden-shell link in a chain, start the tool with `zsh -c '<command>; :'`; the trailing
  `:` stops zsh from exec'ing the command.
- Run fixture steps in a `( ... )` subshell, not `{ ... }`: a `cd` into the fixture inside
  braces moves the calling shell there, and the stop-by-cwd script below then kills it.
- zsh does not word-split `$VAR`, so `kill $PIDS` with a space-separated list fails with
  "illegal pid".
- Processes started with a relative argv (`node node_modules/.bin/vite`) are not found by
  `pkill -f <fixture path>`. Stop them by cwd: read `devdash --json` and send SIGTERM to every
  process whose `cwd` starts with the fixture directory.
- The stop-by-cwd script must not run with its own cwd inside the fixture directory, or it
  finds itself in `devdash --json` and stops itself before it finishes. Run it from `/`, skip
  its own pid and ppid, and compare against the physical path (`pwd -P`): on macOS process
  cwds are under `/private/tmp`, not `/tmp`.
- Never run `devdash kill N --yes`, or confirm a kill in the dashboard, on a port the fixture
  did not open: on macOS 5000 and 7000 are the AirPlay receiver's. To see a refusal, use a port
  held by `OrbStack Helper` or by pid 1 (8021 is launchd's), or run `kill N` without `--yes`
  and with stdin from `/dev/null`.

### Reading drive.py screens

- `drive.py` puts every character in one cell, so a line with double-width characters prints
  one column short per wide character. Judge alignment by display width
  (`unicodedata.east_asian_width`), not by how the printed text looks.
- Filter captured screens with `--size` and `--lines`, not with shell tools: `grep -v claude`
  also drops every row whose command contains the session's scratchpad path, and macOS `cut -c`
  counts bytes, so it cuts lines with `·` or `›` early.
- On a busy machine, other sessions' repositories add rows that shift between screens. Pass
  `--cmd "./devdash --no-color --roots <fixture dir>"` so they go into the collapsed `other`
  group.

### Linux QA in a container

- OrbStack is the container runtime on the maintainer's Mac (`docker info` works while it
  runs). A Linux check runs well in `debian:trixie-slim` with `--init`, git, procps and python3
  installed inside, the repository mounted read-only and the binary cross-built with
  `GOOS=linux GOARCH=arm64`; trixie's git 2.47 supports `--ref-format=reftable`.
- Once built, the image `make demo-docker` uses (`devdash-demo`: git, python3 and `go.mod`'s
  Go) runs a Linux pass with no downloads: mount a directory holding the cross-built binaries
  and `drive.py`, and run it with `--init` and the entrypoint `sleep`.
- For Linux-only code (`*_linux.go`), run
  `go test ./internal/collector ./internal/engine ./internal/freeport` inside that image as uid
  1000, and as root where it matters; `-race` does not work there (no cgo in that image).
- That image has no zsh: a hidden-shell link there uses `bash -c '<command>; :'`, and a fixture
  that waits for a port started under zsh hangs, so wrap the container command in `timeout`.
  macOS has no `timeout` binary; run it inside the container (`--entrypoint timeout`).
- `docker exec` into the container while the dashboard runs adds a transient process that root
  cannot read, so one screen shows the `CAP_SYS_PTRACE` hint. Do not poll the container with
  `docker exec` during a capture.

### make and CI

- `make check 2>&1 | tail` reports tail's exit code; run make unpiped or grep its output for
  `FAIL`.
- `make docker-gate` is not a Linux test run: it is the Phase 3 compose gate, the darwin binary
  against an OrbStack container.
- The Release workflow's macOS job can stay queued until GitHub cancels it ("The job was not
  acquired by Runner of type hosted"), which fails the run with nothing published.
  `gh run rerun <id> --failed` on the same tag finishes the release.
- A release QA for a refactor-only range: build the previous tag and `main` with the same
  `-ldflags` version, copy each in turn to one path, capture the CLI answers and `drive.py`
  screens on one fixture, and diff them with pid, uptime, cpu and mem masked. About 50
  `drive.py` steps take 1.5 minutes per run, so keep to a few sizes.

## Run (for humans)

```bash
make build && ./devdash
```

`?` lists the keys, `q` quits.
