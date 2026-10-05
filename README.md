# devdash

What is running on this machine, and for which project, in one terminal screen.

Forgotten dev servers hold ports, watchers keep running in abandoned worktrees, and agent
sessions pile up. Finding them usually takes `lsof`, `ps`, `docker ps` and some guessing.
devdash groups every process by the git repository it runs in, attaches listening ports and
Docker containers to the processes that own them, and lets you kill any of it. It is one
static Go binary for macOS and Linux, needs no root, and reads the OS directly instead of
shelling out to `lsof`, `ss`, `netstat` or `ps`.

![The devdash dashboard: four repositories grouped with their dev servers, watchers, a test runner and an agent; typing a port number selects its holder, the detail pane explains its tags, a kill frees the port, a folded nodemon chain unfolds and is killed as a tree, and devdash port and devdash free answer from the shell](docs/demo.gif)

## Install

**Homebrew** (macOS and Linux), a cask from the `dogauzun/tap` tap:

```sh
brew install dogauzun/tap/devdash
```

**Go** 1.27.1 or newer (the version in `go.mod`):

```sh
go install github.com/dogauzun/devdash/cmd/devdash@latest
```

**Release archives.** Each [GitHub release](https://github.com/dogauzun/devdash/releases) has
an archive for darwin/arm64, darwin/amd64, linux/amd64 and linux/arm64, and a `checksums.txt`.
An archive holds `devdash`, `LICENSE`, `THIRD_PARTY_LICENSES`, this README and
`docs/usage.md`, `docs/limitations.md` and `docs/json-schema.md`. Check the download, unpack it
and put `devdash` on your `PATH`:

```sh
sha256sum --ignore-missing -c checksums.txt            # Linux
shasum -a 256 --ignore-missing -c checksums.txt        # macOS
```

All binaries are built with `CGO_ENABLED=0` and are static. The macOS build loads libproc at
run time through [purego](https://github.com/ebitengine/purego), so it needs no cgo either.

## Usage

```sh
devdash                     # the dashboard
devdash port 5173           # who listens on TCP port 5173, and the next free port
devdash free 3000           # the first free port from 3000 to 3099
devdash kill 3000 --tree    # stop whatever listens on 3000, and its descendants
devdash --json              # one snapshot as JSON
```

**The dashboard** is one screen, usable at 80 columns by 24 rows. Processes are grouped by
project (`name @ branch (worktree)`) and form a tree, each with a kind: agent, test, watcher,
editor, shell, server, container or other. Processes with no project go under `other`. Type a
port number and `enter` to select whatever holds it, then `x` to kill or `enter` for the detail
pane; `?` shows the keys.

**In scripts**, `port N` exits 0 when something listens and 1 only when the port is free, and
`free N` prints a port you can bind:

```sh
devdash port 3000 || npm run dev
PORT=$(devdash free 3000) npm run dev
```

The answer means free at the moment of the check, not reserved: another process can take the
port before your server starts.

**`kill N`** prints the plan first (every pid with its name, project and ports), then asks for
confirmation. It refuses pid 1, itself and its ancestors, and container-runtime processes, and
a target outside every project asks a second time.

Every command's output, flags and exit codes are in [docs/usage.md](docs/usage.md); the
`--json` schema is in [docs/json-schema.md](docs/json-schema.md). `devdash --help` prints a
summary.

## Keybindings

The dashboard's keys; `?` shows the same table inside it.

| Key | Action |
| --- | --- |
| `↑ ↓ j k` | move the selection |
| `pgup pgdown` | page the table or the open detail pane; g/home first, G/end last row |
| `← → h l` | collapse or expand a group or node; → unfolds a chain, ← refolds |
| `enter` | open or close the detail pane (esc closes it too) |
| `/` | filter by port, name, argv, project, container or tag; esc clears |
| `0-9` | port search: opens the filter with the digit, selects the holder |
| `x` | kill modal: p process, t tree, f force, Y second confirm, esc cancel |
| `o` | open http://localhost:&lt;port&gt; (the lowest port) |
| `a` | show or hide shells and editors |
| `d` | show or hide container rows |
| `s` | cycle sort within groups: default, port, cpu, start time, name |
| `r` | refresh now |
| `S` | rerun under sudo (asks first) when a warning or a kill needs root |
| `?` | this help |
| `q ctrl-c` | quit |

In the kill modal, `enter` or `y` confirms. A target outside every project asks once more and
takes only `Y`. When processes survive, `f` force-kills them.

## Why another port tool

Tools such as [portview](https://docs.rs/portview),
[porthog](https://pypi.org/project/porthog/),
[PortPilot](https://dev.to/abdullahtarakji/building-portpilot-a-modern-tui-for-port-management-2p18),
[killport-tui](https://github.com/last1chosen/killport-tui) and
[somo](https://github.com/theopfr/somo) answer "who has port N", and answer it well. devdash
answers "what is running for project X". A port is one attribute of a process, not the unit
the tool is built around.

| Question | devdash |
| --- | --- |
| Who has port N? | `devdash port N`, or filter the dashboard by port |
| Which processes belong to this repository? | every process is grouped under the git repository of its working directory, linked worktrees included |
| What runs here without a port? | watchers, test runners and agent sessions are shown under their project, listening or not |
| Which container holds this port? | a published port is shown as its container under its compose project, not as `docker-proxy` |
| What exactly will this kill do? | the plan lists every pid before any signal is sent |

If you only want to free a port, any of the tools above will do, and some cover ground devdash
does not: portview can inspect remote hosts over SSH, and killport-tui runs on Windows.

## Known limitations

- **Other users' processes.** Without sudo their argv, cwd, CPU and memory may be unknown and
  their listeners have no owner. devdash warns instead of hiding them, and `S` reruns it under
  sudo. Root in a container without `CAP_SYS_PTRACE` is in the same position, and a Linux
  `hidepid` mount hides those processes entirely.
- **macOS.** Other users' listeners are withheld when an ancestor of devdash is ad-hoc signed
  (for example `go run`). Start devdash directly from a shell, or use sudo.
- **Docker** is optional. Only plain `unix://` and `tcp://` endpoints are supported, not TLS.
- **Container processes on Linux** are listed as ordinary host processes under `other`, and a
  `--network host` container's port belongs to a plain process, which `kill N` signals.
- **UDP and unix sockets** are not shown yet.
- **Windows** is not supported. Neither are remote hosts, a config file or a background daemon.

The details and workarounds are in [docs/limitations.md](docs/limitations.md).

## Building from source

```sh
make build   # ./devdash
make check   # lint, vet, cross-builds for darwin and linux, race tests (what CI runs)
```

`make` lists the other targets. Without make: `CGO_ENABLED=0 go build ./cmd/devdash` and
`go test -race ./...`.

`make demo` re-records the GIF above from [demo.tape](demo.tape) (Linux only, as root).

## Contributing

The spec in [docs/SPEC.md](docs/SPEC.md) is the authority. [CLAUDE.md](CLAUDE.md) has the
layout, commands and conventions (conventional commits, tests first, no cgo, a short
dependency list). Non-obvious choices get one dated line in [DECISIONS.md](DECISIONS.md).

Please report security bugs privately, as [SECURITY.md](SECURITY.md) describes, not in a
public issue.

## License

MIT, see [LICENSE](LICENSE).
