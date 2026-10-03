# devdash

What is running on this machine, and for which project, in one terminal screen.

Forgotten dev servers hold ports, watchers keep running in abandoned worktrees, and agent
sessions pile up. Finding them usually takes `lsof`, `ps`, `docker ps` and some guessing.
devdash groups every process by the git repository it runs in, attaches listening ports and
Docker containers to the processes that own them, and lets you kill any of it. It is one
static Go binary for macOS and Linux, needs no root, and reads the OS directly instead of
shelling out to `lsof`, `ss`, `netstat` or `ps`.

![The devdash dashboard run from a repository, whose group comes first marked (here): four repositories grouped with their dev servers, watchers, a test runner and an agent, and a leftover dev server tagged orphaned and cwd deleted; the detail pane explains its tags, a tree kill removes nodemon and its child, then devdash port 5174 and port 5173 show each holder's command, project, uptime and tags with the next free port, and devdash free 5173 prints 5175](docs/demo.gif)

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
Check the download, unpack it and put `devdash` on your `PATH`:

```sh
sha256sum --ignore-missing -c checksums.txt            # Linux
shasum -a 256 --ignore-missing -c checksums.txt        # macOS
```

All binaries are built with `CGO_ENABLED=0` and are static. The macOS build loads libproc at
run time through [purego](https://github.com/ebitengine/purego), so it needs no cgo either.

## Usage

### The dashboard

```sh
devdash
```

One screen: a header, a table grouped by project, and a footer with key hints and warnings.
Each project header shows `name @ branch (worktree)`. Inside a group, processes form a tree by
parent pid, and each has a kind: agent, test, watcher, editor, shell, server, container or
other. Shells and editors are hidden unless `--all` is given or toggled in the dashboard.
Processes with no project go under `other`, and containers without a compose project under
`containers`. A detail pane shows the full argv, cwd, listeners, parent chain and start time.
It is meant to be usable at 80 columns by 24 rows. `?` lists the keys (see
[Keybindings](#keybindings)) and `q` quits.

### `devdash --json`

Prints one snapshot as a JSON document on stdout. It samples twice, 200 ms apart, so
`cpu_percent` is a real number. Trimmed output on Linux:

```console
$ devdash --json | head -12
{
  "schema_version": 1,
  "taken_at": "2026-10-02T19:12:02.099069847Z",
  "host": {
    "os": "linux",
    "arch": "amd64",
    "hostname": "dev-box",
    "uid": 0
  },
  "projects": [
    {
      "id": "/home/me/code/shop",
```

For scripts, for example every process that listens on a port:

```sh
devdash --json | jq -r '.processes[] | select(.listeners | length > 0) | "\(.pid)\t\(.name)\t\([.listeners[].port] | join(","))"'
```

| Exit code | Meaning |
| --- | --- |
| 0 | the snapshot was printed |
| 2 | usage error |
| 5 | devdash failed: no snapshot could be taken, or stdout could not be written |

### `devdash port N`

Who listens on TCP port N: one line per listener with pid, name, project (`-` for none) and
bind address. A listener whose owner devdash cannot read is pid 0 with a hint. A published
container port shows as the container: the pid of the process forwarding it (`-` when there
is none or devdash cannot see it), the container's name and compose project, the address, its
image and the forwarder.

On a terminal, each process holding the port gets up to three more lines, indented: its full
command (cut to the terminal's width); where it runs (project as `name @ branch`, the working
directory when it is in no repository, or `-`), how long it has been up, and `this repo` or
`this repo, other worktree` when it runs in the repository you ran devdash from or another
worktree of it; and its tags, `orphaned` (its parent exited) or `cwd deleted` (its working
directory is gone), when either applies. Tags state facts; they do not say a process is safe
to kill. When the port is held, the answer ends with the port to use instead, found as
`devdash free N+1` finds it (no line for N = 65535):

```console
$ devdash port 5173
15669  python3  shop  0.0.0.0:5173
       uvicorn app:main --reload --port 5173
       shop @ feat/login (worktree), up 3h, this repo
       orphaned, cwd deleted
next free: 5174
$ devdash port 5432
20  shop-db-1  shop  0.0.0.0:5432  container (postgres:16) via docker-proxy
next free: 5433
$ devdash port 2024
0  unknown  -  0.0.0.0:2024  owner unknown: run with sudo to see owners
next free: 2025
$ devdash port 4999
free
```

Container lines and the unknown-owner line get no extra lines. When nothing from N+1 to N+100
(at most 65535) is free, the last line is `next free: none in 5174-5273`; when the free-port
check itself fails, the line is left out, the reason goes to stderr and the exit code stays.
Piped or redirected, `port N` prints only the listener lines, byte for byte as v0.1.1 did, so
scripts reading them keep working. For scripts that want everything above, `devdash port N
--json` prints the answer as one JSON object (holders, containers, `free` and `next_free`; see
[docs/json-schema.md](docs/json-schema.md#port_answer)), with the same exit codes.

| Exit code | Meaning |
| --- | --- |
| 0 | something listens on N |
| 1 | the port is free |
| 2 | usage error |
| 5 | devdash failed, so it could not look |

Exit 1 only ever means "free", so `devdash port 3000 || npm run dev` never starts a second
server because devdash failed.

### `devdash free N`

Prints the first TCP port from N to N+99 (at most 65535) that nothing listens on, on any
address, and that you can bind, so a dev server can start on another port:

```sh
PORT=$(devdash free 3000) npm run dev
```

A port counts as taken when a listener or a container's published port holds it, or when a
test bind fails, which also catches other users' listeners that devdash cannot see without
root. The answer means free at the moment of the check, not reserved: another process can take
the port before your server starts. On macOS a port in TIME_WAIT (just after a server exited
with open connections) counts as taken.

| Exit code | Meaning |
| --- | --- |
| 0 | the port is printed |
| 1 | nothing is free from N to N+99; nothing is printed |
| 2 | usage error |
| 5 | devdash failed, so it could not look; nothing is printed |

### `devdash kill N`

```sh
devdash kill N [--tree] [--force] [--yes] [--timeout 3s]
```

Stops whatever listens on TCP port N. devdash prints the plan first (mode, signal, and every
pid with its name, project and ports), asks for confirmation on the terminal, signals, waits,
and reports what survived. Then it checks the port again and says whether it is free.

- Default: `SIGTERM` to each owner of the port.
- `--tree`: the owner and all its descendants, parent first, so a supervisor such as nodemon
  or air cannot respawn a child. A process-group leader also gets one signal to its group.
- `--force`: `SIGKILL` instead of `SIGTERM`, for whichever set was chosen.
- `--yes`: do not ask. Without a terminal on stdin, kill needs `--yes`.
- `--timeout d`: how long to wait for the signalled processes to exit (default 3s).

A target outside every project asks a second time, because it is usually a system service.
devdash refuses pid 1, itself and its ancestors (your shell and terminal), and
container-runtime processes such as `dockerd`, `docker-proxy`, `com.docker.backend` and
OrbStack's `OrbStack Helper`. A
port published by a container is refused with a `docker stop <name>` hint. If any owner is
refused, nothing is signalled. Each
pid's start time is checked again right before `kill(2)`, so a reused pid is never signalled.

| Exit code | Meaning |
| --- | --- |
| 0 | every signalled process exited, or nothing listens on N |
| 2 | usage error, or no terminal to confirm on and no `--yes` |
| 3 | permission denied, or the owner is unknown (another user's; try sudo) |
| 4 | survivors remain (try `--force`) |
| 5 | devdash failed before anything was signalled |
| 6 | nothing signalled: a refused target, or the confirmation was declined |

### `devdash version`

Prints the version, the commit, and the commit's date (labelled `built`; release builds and
`go install` builds both record the commit time, not the build time). Exits 0.

### Global flags

Flags may come before or after the command. `-h` or `--help` prints the usage and exits 0.

| Flag | Effect |
| --- | --- |
| `--roots paths` | only count git repositories under these directories; comma-separated and repeatable; a leading `~` is `$HOME` |
| `--tick d` | dashboard refresh interval (default 2s, minimum 500ms) |
| `--all` | show shells and editors in the dashboard (`--json` always lists every process) |
| `--no-docker` | do not ask Docker for containers |
| `--no-color` | no colour; also when `NO_COLOR` is set and not empty |
| `--json` | print one snapshot as JSON |

## Keybindings

The dashboard's keys; `?` shows the same table inside it.

| Key | Action |
| --- | --- |
| `↑ ↓ j k` | move the selection |
| `pgup pgdown` | page the table or the open detail pane; g/home first, G/end last row |
| `← → h l` | collapse or expand a project group or a tree node |
| `enter` | open or close the detail pane (esc closes it too) |
| `/` | filter by port, name, argv, project, container or tag; esc clears |
| `x` | kill modal: p process, t tree, f force, Y second confirm, esc cancel |
| `o` | open http://localhost:&lt;port&gt; (the lowest port) |
| `a` | show or hide shells and editors |
| `d` | show or hide container rows |
| `s` | cycle sort within groups: default, port, cpu, start time |
| `r` | refresh now |
| `?` | this help |
| `q ctrl-c` | quit |

In the kill modal, `enter` or `y` confirms. A target outside every project asks once more and
takes only `Y`. When processes survive, `f` force-kills them.

## JSON output

`--json` follows schema version 1, documented field by field in
[docs/json-schema.md](docs/json-schema.md).

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

- **Other users' processes.** Without sudo, their rows are shown but argv, cwd, CPU and
  memory may be unknown, and their listeners have no owner (pid 0, "owner unknown"). devdash
  shows a warning instead of hiding them. On Linux, root in a container with default
  capabilities (no `CAP_SYS_PTRACE`) is in the same position; `--cap-add SYS_PTRACE` fixes it
  (see [DECISIONS.md](DECISIONS.md), DEV-13).
- **Linux `hidepid`.** With `/proc` mounted `hidepid=1` or `2` (`noaccess` or `invisible`),
  other users' processes are invisible and their listeners stay without an owner. devdash
  warns and names the mount option.
- **macOS socket list.** Other users' listeners come from the kernel's TCP socket list, which
  macOS withholds when an ancestor of devdash is ad-hoc signed (for example `go run`). An
  empty list is treated as unknown, not as "no listeners", and a footer hint says so. Start
  devdash directly from a shell, or use sudo. Your own listeners are always found. See the
  Signing notes in [docs/SPEC.md](docs/SPEC.md#build-release-and-distribution) and [DECISIONS.md](DECISIONS.md) (DEV-10).
- **Docker** is optional. devdash uses `DOCKER_HOST`, then the current docker context, then
  the first socket it finds among Docker Desktop's, OrbStack's, Colima's, `/var/run/docker.sock`
  and Podman's. With no socket there are no container rows and no warning, and the dashboard
  looks again every 10 ticks, rounded up to whole 5 s steps (20 s by default), so Docker
  started after devdash shows up within that time; `port`, `free`, `kill` and `--json` look
  once. An unreachable or slow engine gives a warning and the last container list. Only plain
  `unix://` and `tcp://` endpoints are supported, not TLS. With the userland proxy disabled
  (`--userland-proxy=false`), Docker 28+ holds each published port in `dockerd`, which shows
  as the container when devdash runs as root and as an unknown owner reconciled to the
  container otherwise; a published port with no socket on the host at all (older engines,
  iptables only) shows as a container with no process. With `--no-docker`, or while Docker is
  not found, a published port shows as the process that forwards it (`docker-proxy` on Linux,
  `com.docker.backend` on Docker Desktop for Mac, `OrbStack Helper` on OrbStack, another
  forwarder on Colima or Podman), or with an unknown owner when root holds it. `port N` only
  reports ports listening on the host, not unpublished ports inside a Docker network.
- **Container processes on Linux.** Processes running inside containers are also host
  processes on Linux, so devdash lists them in the `other` group as ordinary processes (with
  host user names for the container's uids), not under their container. A container on
  `--network host` publishes no port, so its listener belongs to a plain process: `port N`
  shows that process, and `kill N` signals it instead of refusing with `docker stop <name>`,
  which stops the container (or restarts it under a restart policy). macOS is unaffected,
  because container processes run inside the VM.
- **UDP and unix sockets** are not shown yet (planned for v1.2).
- **Windows** is not supported. Neither are remote hosts, a config file or a background
  daemon: devdash runs only while its terminal is open.

## Building from source

```sh
make build   # ./devdash
make check   # lint, vet, cross-builds for darwin and linux, race tests (what CI runs)
```

`make` lists the other targets. Without make: `CGO_ENABLED=0 go build ./cmd/devdash` and
`go test -race ./...`.

`make demo` re-records the GIF above from [demo.tape](demo.tape) with
[vhs](https://github.com/charmbracelet/vhs). It runs on Linux only, as root, with vhs, ttyd,
ffmpeg and Chromium installed. [scripts/demo.sh](scripts/demo.sh) sets up the repositories it
shows.

## Contributing

The spec in [docs/SPEC.md](docs/SPEC.md) is the authority. [CLAUDE.md](CLAUDE.md) has the
layout, commands and conventions (conventional commits, tests first, no cgo, a short
dependency list). Non-obvious choices get one dated line in [DECISIONS.md](DECISIONS.md).

Please report security bugs privately, as [SECURITY.md](SECURITY.md) describes, not in a
public issue.

## License

MIT, see [LICENSE](LICENSE).
