# devdash — Technical Specification

> Copy of https://claude.ai/artifact/KAMHVNSXtKDT9fLjxLDryS as of 2026-10-02. Tracked in Jira project DEV.

2026-09-28 · Doga · Status: Draft v0.1

## Summary

devdash answers "what is running on this machine, and for which project" in one terminal screen, and lets you kill any of it. It groups processes by git repository, attaches listening ports and Docker containers to the processes that own them, and ships as one static Go binary for macOS and Linux.

The problem: forgotten dev servers hold ports, watchers keep running in abandoned worktrees, agent sessions pile up, and finding them takes lsof, ps, docker ps and guesswork. Existing tools such as [portview](https://docs.rs/portview), [porthog](https://pypi.org/project/porthog/), [PortPilot](https://dev.to/abdullahtarakji/building-portpilot-a-modern-tui-for-port-management-2p18) and [killport-tui](https://github.com/last1chosen/killport-tui) answer "who has port N". devdash answers "what is running for project X", with ports as one attribute of a process.

This document specifies v1, built as a solo open-source project with Claude Code. Facts marked "verify" come from documentation and one field report rather than a running prototype; the Phase 0 spike in Milestones confirms them before anything is built on top.

## Goals and non-goals

v1 is done when all eight goals hold on both operating systems without root.

- Every listening TCP socket (IPv4 and IPv6) on the machine is shown, with its owning process when that is readable.
- Every process whose working directory resolves to a git repository is shown under that repository, whether or not it listens on a port.
- Docker containers with published ports appear under their compose project, and a published port is attributed to the container rather than to docker-proxy or com.docker.backend.
- A process can be killed alone, with its whole tree, or force-killed, after a confirmation that lists exactly what will receive the signal.
- A full snapshot completes in 100 ms or less on a machine with about 500 processes, without shelling out to lsof, ss, netstat or ps.
- Missing permissions degrade the output by marking fields unknown, never by dropping rows or crashing.
- The same data is available to scripts: `--json` with a versioned schema, `port N` with exit codes, `kill N`.
- Builds are `CGO_ENABLED=0` for darwin/arm64, darwin/amd64, linux/amd64 and linux/arm64.

Non-goals for v1: Windows, remote hosts, a config file, log viewing, starting or restarting processes, Kubernetes, UDP and unix-domain sockets (planned for v1.1), notifications, and any daemon or background service. devdash runs only while its terminal is open.

## Architecture

The TUI and the CLI are thin clients of one engine; the engine owns the refresh loop, builds snapshots from the OS collector and Docker, and exposes the actions.

```
cmd/devdash  ->  engine, tui
tui          ->  engine
engine       ->  collector, model, docker
collector    ->  one implementation per OS (linux, darwin) behind build tags, plus a fake
```

One collector is compiled per OS with build tags; the engine, model and both interfaces are platform-independent and see only the collector interface.

| Package | Responsibility | Depends on |
| --- | --- | --- |
| `cmd/devdash` | flags, subcommand dispatch, exit codes | engine, tui |
| `engine` | refresh loop, snapshot publishing, actions (kill, open) | collector, model, docker |
| `model` | pure functions: process tree, project resolution, kinds, Docker reconciliation, filter, row flattening | standard library only |
| `collector` | `Collector` interface, `linux` and `darwin` implementations, `fake` for tests | `golang.org/x/sys/unix`, `purego` (darwin) |
| `docker` | socket discovery, minimal Engine API client | `net/http` |
| `tui` | Bubble Tea v2 program, views, key handling | engine |

Threading: one goroutine runs the ticker (2 s by default) and calls the collector with a 1.5 s context timeout; the Docker client runs on its own 5 s cadence in a second goroutine. Each finished snapshot is published on a channel of size 1 where the latest value wins, and the TUI subscribes through a `tea.Cmd`; the UI goroutine never blocks on collection. Actions run synchronously from the caller with a 3 s timeout and trigger an immediate refresh.

## Data model

A `Snapshot` is an immutable value produced once per tick; everything the TUI, the JSON output and the actions need is derived from it, so the two interfaces cannot disagree.

```go
type Snapshot struct {
    SchemaVersion int        // 1
    TakenAt       time.Time
    Host          Host       // OS, arch, hostname, uid devdash runs as
    Processes     []Process
    Projects      []Project
    Containers    []Container
    Warnings      []Warning  // deduplicated: code, count, hint
    Timing        Timing     // per-source durations, for --json and the footer
}

type Process struct {
    PID, PPID   int
    StartTime   time.Time  // identity is (PID, StartTime); PIDs are reused
    UID         int
    User        string
    Name        string     // comm on Linux, p_comm on macOS (16 chars), argv[0] basename when available
    Argv        []string
    Cwd         string     // "" when unreadable
    CPUPercent  float64    // delta between consecutive samples; NaN on the first
    RSSBytes    uint64
    Listeners   []Listener
    Kind        Kind
    ProjectID   string     // Project.ID or ""
    ContainerID string     // set when every published port this process holds is one container's
    Unknown     FieldSet   // which fields could not be read (argv, cwd, cpu, mem, owner)
}

type Listener struct {
    Proto string      // "tcp4" | "tcp6"
    Addr  netip.Addr  // bind address; unspecified means every interface
    Port  uint16
    ContainerID string // the container whose published port this is, "" otherwise
}

type Project struct {
    ID       string  // repository root path, also the group key
    Root     string
    Name     string  // basename of Root, or of MainRepo for linked worktrees
    Branch   string  // "" when detached; then ShortSHA is set
    ShortSHA string
    Worktree bool
    MainRepo string  // for linked worktrees only
}

type Container struct {
    ID, Name, Image string
    State          string  // running, paused, restarting ...
    ComposeProject string  // label com.docker.compose.project, "" when absent
    ComposeService string
    Ports          []PortMapping  // HostIP, HostPort, ContainerPort, Proto
}

type Kind uint8 // KindOther, KindServer, KindContainer, KindAgent, KindTest, KindWatcher, KindShell, KindEditor
```

Three rules keep the model honest. A listener that no readable process owns still exists in the snapshot as a `Process` with `PID` 0, `Name` "unknown" and the `owner` bit set in `Unknown`, so the port is never hidden. A container is represented twice on purpose: once in `Containers` for the compose view, and once through `ContainerID` on the proxy process that holds its port, so a kill on that row is refused with a pointer to `docker stop`. Every field that can fail independently has an `Unknown` bit rather than a sentinel value, and the JSON output lists those bits by name.

## Platform data sources

Everything is read from kernel interfaces directly; lsof, ss, netstat and ps never run on the refresh path. The table shows what each facet costs in permissions; "own uid" means processes running as the same user as devdash.

| Facet | Linux, own uid | Linux, other uid | macOS, own uid | macOS, other uid |
| --- | --- | --- | --- | --- |
| pid, ppid, uid, name, start time | yes | yes | yes | yes |
| argv | yes | yes (world-readable unless `hidepid`; verified 2026-10-02) | yes | no (root only; `kern.procargs2` fails with EINVAL) |
| cwd | yes | no | yes | no (EPERM) |
| which TCP ports are listening | yes (world-readable) | yes, with the socket's uid, also under `hidepid` | yes | from the PCB list, which macOS 27 withholds (empty, no error) when any ancestor of devdash is ad-hoc signed |
| owning pid of a listener | yes | no | yes | `so_last_pid` from the PCB list when served (matched lsof on every listener) |
| CPU time, RSS | yes | yes | yes | no (`PROC_PIDTASKINFO` fails with EPERM) |

**Linux.** The process list is the numeric entries of `/proc`. Per process: `/proc/[pid]/stat` gives ppid, state, utime, stime and starttime (clock ticks since boot at USER_HZ = 100, added to `btime` from `/proc/stat`); `/proc/[pid]/status` gives the effective uid (second field of `Uid:`), as macOS reports it; `/proc/[pid]/cmdline` is NUL-separated argv (empty for kernel threads, zombies, processes that blanked their argv, and briefly for a process in the middle of exec, whose comm is already the new program's; kernel threads, recognised by `PF_KTHREAD` in the stat flags, and zombies, state `Z` or `X`, are skipped, while a user process with an empty argv is kept); `/proc/[pid]/statm` gives resident pages; `readlink /proc/[pid]/cwd` fails with EACCES for other users' processes, and appends ` (deleted)` to a removed directory, which is stripped so both OSes report the old path. Listeners come from `/proc/net/tcp` and `/proc/net/tcp6`: rows with state `0A`, local address and port in hex (the address is printed as 32-bit words in host byte order), the socket's uid in the 8th column and its inode in the 10th (not the last: refcount, pointer and timer columns follow). A v4-mapped address in `tcp6` (`::ffff:127.0.0.1`) is reported as `tcp4 127.0.0.1`, as on macOS. Ownership is the inode: `readlink /proc/[pid]/fd/*` yields `socket:[inode]`, which is scanned in pid order (a socket shared across fork goes to the lowest pid) only for pids of our own uid (others fail anyway; all pids when running as root) and only while listeners whose socket uid is our own (all of them as root) remain unmatched. `/proc/net/*` is per network namespace, so sockets inside containers are not visible; their published ports appear as docker-proxy and are reconciled through the Docker API instead.

**macOS.** The process list and its basic facets come from one `sysctl kern.proc.all` call (`unix.SysctlKinfoProcSlice` in `golang.org/x/sys/unix`): pid, ppid, uid, start time and the 16-character `p_comm`. Everything else comes from libproc, loaded with purego so the build stays `CGO_ENABLED=0`, which is how [gopsutil v4](https://github.com/shirou/gopsutil/blob/master/process/process_darwin.go) does it and where the struct layouts can be checked: `proc_pidinfo(PROC_PIDVNODEPATHINFO)` for cwd, `proc_pidinfo(PROC_PIDTASKINFO)` for CPU time and resident size, `proc_pidinfo(PROC_PIDLISTFDS)` then `proc_pidfdinfo(PROC_PIDFDSOCKETINFO)` per socket fd for listeners (kind `SOCKINFO_TCP`, state `TSI_S_LISTEN`, local port and family). argv comes from `sysctl kern.procargs2`. All of these work for our own uid without root.

For other users' listeners on macOS the only host-wide source is `sysctl net.inet.tcp.pcblist_n`, whose records carry a bind address, port, state and `so_last_pid`. One tool's README reports that on macOS 27 an ad-hoc-signed binary receives an empty PCB list while Apple-signed netstat still sees the listeners ([osfacts](https://github.com/juspay/osfacts)). The spike (macOS 27.0.1) found the gate follows process ancestry, not the caller's own signature: an ad-hoc-signed devdash started from a shell gets the full list, while any process with an ad-hoc-signed ancestor (`go run`, `go test`, and every child of an ad-hoc devdash, Apple-signed netstat and sysctl included) gets only the headers and its own sockets, with no error. The design does not depend on it: the per-uid fd walk is the primary source and an empty PCB list is treated as unknown rather than as no listeners. There is no netstat fallback: a `netstat -anv` spawned by an ad-hoc-signed devdash inherits the restriction and sees nothing.

Why not gopsutil for sockets: its darwin and FreeBSD connection lookups shell out to lsof ([gopsutil PR 1551](https://github.com/shirou/gopsutil/pull/1551)), which is slow on a busy machine and has broken on warnings written to stderr. gopsutil remains a reference for the darwin struct layouts and may be used for CPU and memory facets if that saves real time.

## Project resolution

A process belongs to the nearest git repository above its working directory; when that is unknown, the parent chain and argv are tried before giving up. The whole procedure is a pure function of the snapshot plus a small cache, so it is unit-tested with temporary repositories.

1. Take the process cwd. If it is unreadable, go to step 5.
2. Walk up from cwd until a directory contains a `.git` entry. A `.git` directory is a main repository. A `.git` file is a linked worktree: read its `gitdir:` line, and the main repository is the directory two levels above the `worktrees/<name>` path it points to.
3. Stop at the filesystem root or at `$HOME`; a repository rooted at `$HOME` (dotfiles) does not count as a project, so the walk continues past it as if no `.git` were there.
4. The repository root is the project ID. Its name is the basename of the root, or of the main repository for a worktree, shown as `shop @ feat/cart (worktree)`.
5. If no project was found, repeat steps 2 to 4 for the cwd of the parent, grandparent and great-grandparent (a daemon that changed directory after being started from a shell inside the repository).
6. Still nothing: take the first absolute path in argv that lies inside a project already found in this snapshot, and use that project. Paths that are not inside a known project are not walked, to avoid stat calls on arbitrary strings.
7. Otherwise the process goes under the `other` group, which is sorted last.

Branch comes from the repository's `HEAD` (`ref: refs/heads/<branch>`, or a 7-character SHA when detached), read from `.git/HEAD` for a main repository and from `<gitdir>/HEAD` for a worktree. Nested repositories and submodules resolve to the nearest `.git`, which is what a developer working inside the submodule expects.

Resolution results are cached per directory path with the mtime of the `.git` entry and of `HEAD`; a hit costs one `stat`, a miss costs the walk, and the cache is capped at 4096 entries. Every filesystem read uses `os.Lstat` and refuses to follow a symlink out of the walked path, so a process running in a symlinked directory still resolves to the real repository.

## Process tree and kinds

Within a project group, rows form a tree by ppid, and each process gets one kind from a small ordered rule table; the first matching rule wins.

The tree is built from the snapshot's pid → ppid map, restricted to processes in the same project. A process whose parent is outside the project (or is pid 1 or launchd after a re-parent) becomes a root of that group. Roots are sorted with listeners first, then by start time; children keep their parent's order. Kernel threads on Linux and processes with an empty argv are never rows.

| Kind | Rule (matched against the basename of argv[0], then the rest of argv) | Examples |
| --- | --- | --- |
| container | set by Docker reconciliation, not by name | docker-proxy, com.docker.backend holding a published port |
| agent | basename in the agent list | claude, codex, aider, gemini, cursor-agent |
| test | basename or argv[1] in the test list | `go test`, jest, vitest, pytest, `cargo test`, rspec |
| watcher | basename in the watcher list | nodemon, air, watchexec, fswatch, entr, `tsc --watch` |
| editor | basename in the editor list, or ends with `-language-server`, or is a known LSP | code, cursor, nvim, vim, emacs, gopls, tsserver, rust-analyzer |
| shell | basename in the shell list | zsh, bash, fish, sh, nu |
| server | has at least one listener and matched nothing above | node, python, java, a Go binary with an HTTP port |
| other | everything else | git, tmux, cron jobs |

The lists live in one Go file with a comment per entry so contributors can extend them without touching logic. The default view hides `shell` and `editor`; a hidden process whose descendant is visible is still drawn, dimmed, so the tree stays connected. `a` in the TUI and `--all` in the CLI show everything.

## Docker integration

Docker is optional input: two GET requests over the engine's unix socket, a 500 ms timeout, and silence when the socket is absent.

Socket discovery, in order: `DOCKER_HOST` when set (unix and tcp URLs); the endpoint of the current docker context (`~/.docker/config.json` → `currentContext`, then that context's `meta.json`); then the first existing path among `~/.docker/run/docker.sock` (Docker Desktop), `~/.orbstack/run/docker.sock`, `~/.colima/default/docker.sock`, `/var/run/docker.sock`, `$XDG_RUNTIME_DIR/podman/podman.sock` and `~/.local/share/containers/podman/machine/podman.sock`. The chosen path is shown in the detail pane's footer so a wrong pick is visible.

The client is `net/http` with a custom `DialContext` on the socket, and requests are versioned at `/v1.41/` so that Docker 20.10 and newer and Podman's compatibility API both answer. `GET /_ping` runs once at start and after each failure; `GET /containers/json` runs every 5 s on its own goroutine. Only these fields are read: `Id`, `Names`, `Image`, `State`, `Labels["com.docker.compose.project"]`, `Labels["com.docker.compose.service"]`, and `Ports[].{IP, PrivatePort, PublicPort, Type}`. The moby client library is not imported; it would multiply the dependency tree for two endpoints.

Reconciliation runs in `model` after every collector snapshot: a listener matches a published tcp mapping when the ports are equal, the listener's bind address equals the mapping's host IP or, failing that, is unspecified (an unspecified listener prefers a mapping on every interface; an empty host IP, as Podman reports it, means every interface), and either the listener's process name is one of `docker-proxy`, `com.docker.backend`, `com.docker.vpnkit`, `vpnkit`, `gvproxy`, `rootlesskit`, `rootlessport`, `slirp4netns`, `pasta`, `limactl`, or the listener's owner is unknown (root-owned docker-proxy on Linux). A matched listener gets `ContainerID` and its process kind `container`. The process gets `ContainerID` only when all its matched listeners are one container's: it is then displayed with the container name and image instead of the proxy's name, under the container's compose project, or under `containers` when it has no compose label. Docker Desktop's `com.docker.backend` holds every container's ports in one process, so when it holds two or more containers' ports it keeps its own row and each container gets a container row. An unmatched published port (userland proxy disabled, iptables-only) still appears as a container row with no process behind it.

Failure handling: a missing socket produces no warning at all; a socket that exists but does not answer produces one footer hint ("docker: not reachable at <path>") and a retry every 10th tick; a request slower than 500 ms keeps the previous container list for this tick.

## Actions

Every destructive action shows what it will do first, refuses a fixed set of targets, and reports what survived.

**Kill.** Three modes, chosen in the confirmation modal (`p`, `t`, `f`) or by CLI flags:

- process: `SIGTERM` to the selected pid.
- tree: `SIGTERM` to the selected pid and all its descendants, parent first so a supervisor such as nodemon or `air` cannot respawn a child that was killed before it. When the selected process is a process-group leader (`pgid == pid`), the signal is also sent to `-pgid` so group members outside the visible tree (a shell's job) are covered.
- force: the same set with `SIGKILL`.

After signalling, the engine polls every 100 ms for up to 3 s (`--timeout`), then reports survivors by pid and name; the TUI offers force on the survivors. Descendants are computed from the latest snapshot's ppid map at the moment of the action, and pids are re-validated against their start time before each `kill(2)` so a recycled pid is never signalled.

**Refused targets.** pid 0, pid 1 (`init`, `launchd`), devdash's own pid, and every ancestor of devdash (the shell and terminal running it). A row that represents a container port is refused with the hint `docker stop <name>`. A process of the container runtime (`dockerd`, `containerd` and its shims, `docker-proxy`, `com.docker.backend`, `com.docker.vpnkit`, `vpnkit`, `gvproxy`, `rootlesskit`, `rootlessport`, `slirp4netns`, `limactl`, `pasta`, `conmon`; the list lives next to the kind lists) is refused by name, whether or not Docker answered, with a hint to find the container: `docker ps --filter publish=<port>` when it is Docker's and holds one port, otherwise `docker ps`, `podman ps` or both, by runtime. Killing it stops other containers or leaves a published port dead. A process owned by another user is attempted, and the resulting `EPERM` is shown as "permission denied, run with sudo". Killing a process outside any project group asks for a second confirmation, because those are usually system services.

**Confirmation.** The modal lists every pid that will receive the signal with its name, project and ports, and the mode. `--yes` skips it on the CLI only, together with the second confirmation for a target outside every project; the TUI always confirms. Without `--yes` the CLI asks on the terminal and, when stdin is not a terminal, exits 2 without signalling.

**`kill N` on the CLI.** N is a TCP port. Every distinct owner of a listener on it gets its own plan; the plans are shown together and confirmed once, and if any owner is refused (including an unknown PID 0 owner), nothing is signalled. After the wait the port is checked again in a fresh snapshot: a forked child can still hold a socket credited only to its parent, so the CLI names the holder and suggests `--tree`; the exit code still describes only the processes that were signalled.

**Open in browser.** `o` runs `open` on macOS or `xdg-open` on Linux with `http://localhost:<port>`, using the lowest port when the process has several. No HTTPS detection in v1; a wrong scheme costs the user one click.

## CLI and JSON schema

The CLI exposes the same snapshot the TUI shows, and its exit codes are the contract scripts rely on.

| Command | Output | Exit code |
| --- | --- | --- |
| `devdash` | the TUI | 0 |
| `devdash --json` | one snapshot as a JSON document on stdout | 0; 5 devdash failed |
| `devdash port 3000` | owner line(s): pid, name, project, bind address; or `free` | 0 found, 1 free, 5 devdash failed |
| `devdash kill 3000 [--tree] [--force] [--yes] [--timeout 3s]` | the plan (mode, signal, every pid with name, project and ports), then what was signalled, survivors, and whether the port is free | 0 all exited (or nothing listens on the port), 2 usage error or no terminal to confirm on without `--yes`, 3 permission denied (or the owner is unknown), 4 survivors remain, 5 devdash failed before anything was signalled, 6 nothing signalled: a refused target or a declined confirmation |
| `devdash version` | version, commit, build date | 0 |

Global flags: `--roots <paths>` limits project scanning to repositories under those directories (comma-separated, and repeatable; a leading `~` is `$HOME`, `~user` is not supported, and a path that is not an existing directory is a usage error); `--tick <duration>` sets the refresh interval (default 2s, minimum 500ms; a smaller value is a usage error); `--no-docker` skips the Docker client; `--all` includes shells and editors in the TUI (`--json` always lists every process); `--no-color` and `NO_COLOR` disable colour. Flags may come before or after the subcommand. Usage errors exit 2 and print to stderr; stdout stays clean for `--json`. `-h` prints the usage on stdout and exits 0. Every command exits 5 when devdash itself fails (no snapshot could be taken, or the output could not be written), with the error on stderr and nothing on stdout; `kill` only before anything was signalled, since after that its code reports the signals; 1 is only ever "port free".

The JSON document carries `schema_version`, and any field removal or rename bumps it. Additions do not. Times are RFC 3339 in UTC, sizes are bytes, durations are milliseconds, and unreadable fields are listed by name in `unknown` rather than filled with placeholders. Every key is always present; an absent or unreadable value is `null`. `--json` samples twice, 200 ms apart, so `cpu_percent` is a number (top-style: one core is 100). The full field reference is [`docs/json-schema.md`](json-schema.md); the example below follows it.

```json
{
  "schema_version": 1,
  "taken_at": "2026-09-28T10:44:03Z",
  "host": {"os": "darwin", "arch": "arm64", "hostname": "mbp", "uid": 501},
  "projects": [
    {"id": "/Users/me/code/shop", "root": "/Users/me/code/shop", "name": "shop",
     "branch": "feat/cart", "short_sha": null, "worktree": true, "main_repo": "/Users/me/code/shop-main"}
  ],
  "processes": [
    {"pid": 48211, "ppid": 48190, "start_time": "2026-09-28T09:12:44Z",
     "uid": 501, "user": "me", "name": "node",
     "argv": ["node", "node_modules/.bin/vite", "--port", "5173"],
     "cwd": "/Users/me/code/shop", "cpu_percent": 0.4, "rss_bytes": 187563008,
     "listeners": [{"proto": "tcp6", "addr": "::", "port": 5173, "container": null}],
     "kind": "server", "project": "/Users/me/code/shop", "container": null,
     "unknown": []},
    {"pid": 0, "ppid": null, "start_time": null, "uid": null, "user": null, "name": "unknown",
     "argv": null, "cwd": null, "cpu_percent": null, "rss_bytes": null,
     "listeners": [{"proto": "tcp4", "addr": "0.0.0.0", "port": 631, "container": null}],
     "kind": "other", "project": null, "container": null, "unknown": ["owner", "argv", "cwd", "cpu", "mem"]}
  ],
  "containers": [
    {"id": "9f1c2a7b0d3e", "name": "shop-db-1", "image": "postgres:16", "state": "running",
     "compose_project": "shop", "compose_service": "db",
     "ports": [{"host_ip": "0.0.0.0", "host_port": 5432, "container_port": 5432, "proto": "tcp"}]}
  ],
  "warnings": [{"code": "listener_owner_unreadable", "count": 3, "hint": "run with sudo to see owners"}],
  "timing_ms": {"argv_cwd": 8.012, "listeners": 1.5, "pcblist": 0.09, "proctable": 0.29, "projects": 1.1, "total": 11.4}
}
```

## TUI design

One screen: a header line, a tree table grouped by project, and a footer with key hints and warnings; a detail pane opens on demand. It must be usable at 80 columns by 24 rows.

**Header.** Hostname, snapshot age ("2 s ago", turning to "stale 8 s" past two missed ticks), counts of projects, listeners and containers, and the active filter.

**Table.** Columns in priority order: name (indented by tree depth, container rows carry the container name and image), kind, ports (comma-joined, with a leading `*` when bound to every interface), pid, uptime, cpu, mem, user, command (truncated to the remaining width). Below 100 columns cpu, mem and user are dropped; below 90 the command is dropped. Project headers show `name @ branch (worktree)`, the process count and the port count, and collapse with `←`. Groups are ordered by most recent activity (latest start time in the group), `containers` and `other` last.

**Detail pane.** `enter` opens it as a right split at 120 columns or more, otherwise as a full-screen overlay: full argv (wrapped), cwd, project and branch, listeners with bind address, parent chain up to the root, start time, user, and the Docker socket in use when the row is a container.

| Key | Action |
| --- | --- |
| `↑` `↓` `j` `k` | move the selection |
| `←` `→` `h` `l` | collapse or expand a project group or a tree node |
| `enter` | open or close the detail pane |
| `/` | filter by port, name, argv, project or container; `esc` clears |
| `x` | kill modal: `p` process, `t` tree, `f` force, `esc` cancel |
| `o` | open `http://localhost:<port>` |
| `a` | show or hide shells and editors |
| `d` | show or hide container rows |
| `s` | cycle sort within groups: default, port, cpu, start time |
| `r` | refresh now |
| `?` | help overlay |
| `q` `ctrl-c` | quit |

**Refresh and selection.** The model keeps the latest snapshot and a flattened row list computed from it. Selection is stored as a row key, `(pid, start_time)` for a process or the project ID for a header, never as an index; after a new snapshot the key is looked up again, and if it is gone the selection moves to the row that now occupies the nearest previous index. Expansion state is a map keyed the same way and survives refreshes. The filter runs on the flattened rows and keeps the ancestors of every match visible so the tree never shows a child without its parent. All of this is pure and tested by sending messages to the model and asserting on `View()`.

**Style.** Bubble Tea v2 with Lip Gloss adaptive colours: one accent for the selection, dimmed text for hidden-but-connected rows, a warning colour for `stale` and for permission hints. No emoji, no box-drawing beyond the table borders, and everything readable with colour disabled.

## Performance and degraded modes

The budget is 100 ms for a full snapshot at about 500 processes, and the refresh loop is designed so that missing the budget degrades freshness, never responsiveness. The per-source targets below are estimates to be confirmed in the Phase 0 spike, not measurements.

| Source | Linux target (ms) | macOS target (ms) | Main cost |
| --- | --- | --- | --- |
| process table, 500 processes | 25 | 20 | 1500 small `/proc` reads vs one sysctl |
| argv and cwd, own uid | 15 | 20 | one read or one libproc call per process |
| listeners and owners | 15 | 30 | fd walk over own-uid pids only |
| project resolution | 5 | 5 | cached; a miss is a directory walk |
| Docker (separate cadence) | 10 | 10 | one HTTP request every 5 s |
| total, p50 | 60 | 75 | budget 100, timeout 1500 |

Rules for the loop: collection runs with a context timeout of 1.5 s; a timed-out tick keeps the last good snapshot and the header shows `stale N s`. When three consecutive ticks exceed 500 ms the interval doubles, up to 10 s, and a footer hint says so; it halves back when ticks are fast again. CPU percent is the delta of process CPU time over the delta of wall time between two consecutive samples of the same `(pid, start_time)`, shown as `–` on the first sample. On Linux the fd walk skips pids whose `/proc/[pid]/fd` failed with EACCES in the previous tick unless the set of unmatched listener inodes changed. The snapshot is built into fresh slices each tick; nothing is mutated in place, so the UI can hold the previous snapshot without locks.

| Condition | Behaviour |
| --- | --- |
| cwd, argv, fd or task info unreadable (other uid) | row shown, fields marked unknown, one warning with a count, footer hint `run with sudo` (as Linux root, which sudo cannot help: names the missing CAP_SYS_PTRACE, `--cap-add SYS_PTRACE`; with it, names a security module or sandbox; an unowned listener when no fd read was denied is outside devdash's pid namespace or held by the kernel, and the hint says that) |
| macOS PCB list empty or denied | other users' listeners treated as unknown (own-uid listeners still come from the fd walk); one warning; hint in footer; no netstat fallback |
| Docker socket absent | no Docker rows, no warning |
| Docker socket present but unreachable or slow | previous container list kept, one footer hint, retry every 10th tick |
| process exits during collection | skipped silently; a partially read process is dropped rather than shown half-filled |
| more than 5000 processes | interval starts at 5 s, argv is read only for processes in a project or with a listener |
| terminal narrower than 80 columns | table keeps name, ports and pid only; detail pane becomes an overlay |
| Linux `/proc` mounted with `hidepid=1` or `2` (shown as `noaccess` / `invisible` since kernel 5.8) | other users' processes invisible, their listeners stay without an owner; one warning naming the mount option |

## Testing strategy

The model is pure and tested exhaustively; the collectors are tested against fixtures for parsing and against the live OS for one known process, which is the test process itself.

| Layer | What is tested | How |
| --- | --- | --- |
| `model` | tree building, project resolution, kinds, Docker reconciliation, filter, row flattening, selection keys | table-driven unit tests; temporary repositories and linked worktrees created with `git` in `TestMain` (skipped when `git` is absent) |
| `collector/linux` parsers | `stat`, `status`, `cmdline`, `statm`, `/proc/net/tcp`, fd links | a fixture proc root under `testdata/proc-500` with hand-written files, including kernel threads, a zombie, a process with EACCES and IPv6 listeners |
| `collector/darwin` decoders | `kinfo_proc`, `proc_taskinfo`, `socket_fdinfo`, `xtcpcb_n` layouts | byte blobs recorded from real calls on macOS 14 and the newest macOS, checked into `testdata` with the OS version in the name |
| collectors, live | the test process is found with its own pid, cwd, argv and a listener opened with `net.Listen` | OS-specific tests run in CI on both runners; a child process started with `exec.Command` is found under the test's pid |
| `docker` | discovery order, request shape, field parsing, reconciliation input | an `httptest.Server` on a temporary unix socket serving canned `/containers/json` responses for Docker and Podman |
| actions | process, tree and force kill; refused targets; survivor reporting | a child tree spawned as `sh -c 'sleep 300 & sleep 300 & wait'`; never a pid the test did not create |
| `tui` | key handling, filter, kill modal, collapse, selection after refresh, narrow widths | `tea.Msg` sequences against the model, golden files of `View()` at 80×24 and 120×40 |
| performance | snapshot time | `BenchmarkSnapshot` over the 500-process fixture; CI records it, does not fail on it |

CI is GitHub Actions on `ubuntu-latest` and `macos-latest`: `go vet`, `golangci-lint`, `go test -race ./...`, `go build` for all four targets, and `goreleaser --snapshot` on every push to check the release pipeline. Every bug fix adds a fixture or a test case that reproduces it first.

## Build, release and distribution

One static binary per target, built by goreleaser from a git tag, installable with Homebrew or `go install`.

- Toolchain: latest stable Go, `CGO_ENABLED=0`, `-trimpath`, `-ldflags "-s -w -X main.version=... -X main.commit=... -X main.date=..."`. The darwin build needs no cgo because libproc is loaded at runtime with purego; if that turns out to be impossible for some call, the fallback is a cgo build for darwin only, kept behind a build tag, and noted in `DECISIONS.md`.
- Targets: `darwin/arm64`, `darwin/amd64`, `linux/amd64`, `linux/arm64`. Windows is a non-goal; the collector interface leaves room for it.
- Dependencies, kept deliberately short: `charm.land/bubbletea/v2` (v2.0.0 shipped 24 February 2026 and the line is at v2.0.10 as of 24 September, per the [release page](https://github.com/charmbracelet/bubbletea/releases)), Lip Gloss and Bubbles at whichever line matches the Bubble Tea major in use, `golang.org/x/sys`, `github.com/ebitengine/purego` (darwin only). No Docker SDK, no gopsutil in the build graph.
- Release: goreleaser builds archives, checksums and a changelog from conventional commits, publishes a GitHub release, and updates a Homebrew cask in a `homebrew-tap` repository (a cask rather than a formula since goreleaser v2.16 deprecated formulas for prebuilt binaries; casks install on macOS and Linux). `go install github.com/<owner>/devdash/cmd/devdash@latest` works from the first tag.
- Versioning: semver; `0.x` until the JSON schema and keybindings have been stable for two releases.
- Signing: Go's linker ad-hoc signs darwin/arm64 binaries, which is enough to run; notarization is not needed for a CLI. macOS 27 withholds the PCB list from any process with an ad-hoc-signed ancestor, and devdash's own signature does not matter when it is launched from a shell. So the per-uid fd walk stays the primary source. The PCB list adds other users' listeners when the kernel serves it, an empty list is "unknown" with a footer hint, and there is no netstat fallback.
- Repository contents on day one: `README.md` (install, keybindings, a short "why another port tool" comparison, demo GIF), `CLAUDE.md` (build, test and lint commands, conventions), `DECISIONS.md` (one line per non-obvious choice), `demo.tape` for [vhs](https://github.com/charmbracelet/vhs), `.goreleaser.yaml`, `.golangci.yml`, `LICENSE` (MIT).

## Milestones

Six phases in strict order, each closed by a gate that is a test or a measurement rather than a feeling; the spike comes first because everything else rests on the platform claims it checks.

| Phase | Scope | Gate |
| --- | --- | --- |
| 0 · Spike | `devdash --json` on this machine, timings to stderr | snapshot in 100 ms or less, root-less behaviour confirmed on macOS and Linux |
| 1 · Collector and model | process table, listeners, projects, kinds, tests | tests green on both operating systems in CI with the race detector on |
| 2 · CLI | `--json` (schema v1 frozen), `port N`, `kill N` | JSON schema documented, exit codes fixed, kill verified on a spawned tree |
| 3 · Docker | socket discovery, `/containers/json`, reconciliation | a compose service's published port shows its container name on both OSes |
| 4 · TUI | table, filter, detail pane, kill modal | usable at 80x24, selection survives a refresh, golden views pass |
| 5 · Release 0.1 | goreleaser, Homebrew tap, README, demo GIF | — |

No phase starts until the previous gate passes; a failed gate sends the work back into the same phase, and a gate that cannot pass (for example the PCB list is empty on macOS 27) records the finding in DECISIONS.md and adjusts scope before moving on. Phases 0 to 2 are the first weekend of work with Claude Code; each phase is one or more small commits, with the plan for the phase written and approved before implementation.

## Risks and open questions

The two risks that could change the design are both about macOS, and both are settled by the spike before any UI code exists.

| # | Risk or assumption | If wrong | Mitigation |
| --- | --- | --- | --- |
| 1 | macOS 27 serves an empty PCB list to processes with an ad-hoc-signed ancestor (verified on 27.0.1; ad-hoc devdash launched from a shell is served) | other users' listeners are invisible without sudo | per-uid fd walk is primary; the PCB list adds other users' listeners when served; an empty list is unknown with a footer hint; no netstat fallback (it inherits the restriction); sudo documented |
| 2 | purego can load libproc without cgo and the `proc_*` struct layouts are stable from macOS 13 to 27 | darwin collector breaks on some version | layouts taken from gopsutil v4 and XNU headers; recorded fixtures per OS version; cgo build behind a tag as last resort |
| 3 | `kern.procargs2` refuses other users' processes without root | argv unknown on those rows | show the 16-character `p_comm`, mark argv unknown |
| 4 | the 100 ms budget with per-pid fd walks | refresh feels sluggish on busy machines | walk own-uid pids only, cache misses on Linux, adaptive interval |
| 5 | Bubbles and Lip Gloss v2 lines are not stable when the TUI phase starts | UI rework later | decide in Phase 4; the v1 line for all three is acceptable |
| 6 | OrbStack, Colima and Podman hold published ports in processes not in the proxy-name list | container ports show as unknown processes | reconciliation also matches on port mapping alone when the owner is unknown; list extended per fixture |
| 7 | crowded field (portview, porthog, PortPilot, killport-tui, somo) | low adoption | project-centric scope, a README comparison, a good demo GIF |
| 8 | tree kill of a supervisor (nodemon, air) races the respawn | orphaned servers survive | parent signalled first, then descendants; survivors reported and re-killable |
| 9 | pid reuse between snapshot and kill | wrong process signalled | `(pid, start_time)` re-validated before every `kill(2)` |
| 10 | Linux `hidepid=2` or containers without userland proxy | rows missing or ownerless | warnings name the cause; documented limitation |

Open questions, to decide before Phase 2 (decisions recorded 2026-10-02, DEV-20):

- The name. `devdash` is generic; check GitHub, Homebrew, pkg.go.dev and crates.io before the first tag. **Decided (2026-10-02): keep `devdash`.** It collides with `Phantas0s/devdash` (a Go terminal dashboard, ~1,600 stars) and is taken on npm and PyPI, and is free on Homebrew core and crates.io; installs are namespaced (`dogauzun/tap/devdash`, `github.com/dogauzun/devdash`), so the collision only affects search.
- Should root-owned listeners on ports below 1024 (sshd, cups, mDNS) be hidden by default behind a `--system` flag, to keep the default view about development? **Decided: no `--system` flag.** Goal 1 shows every listening TCP socket; root-owned listeners below 1024 stay visible in the `other` group.
- Default interval: 2 s, or 1 s with the adaptive backoff carrying the load? **Decided: 2 s**, with the adaptive backoff.
- Is UDP worth including in v1 after all, given the fd walk already sees UDP sockets on both OSes? **Decided: no**, UDP stays a v1 non-goal; planned for v1.1.
- Should `port N` also answer for a port held inside a Docker network but not published, using the container's port list? **Decided: no.** `port N` answers only for ports listening on the host, including published container ports; unpublished ports inside a Docker network are not reported.

## Appendix: prior art

Every tool below is port-centric; none groups by repository or shows non-listening processes of a project, which is the gap devdash fills.

| Tool | Language, UI | Data source | Docker | Notable |
| --- | --- | --- | --- | --- |
| [portview](https://docs.rs/portview) | Rust, CLI + `watch` TUI | reads the OS directly: `/proc` on Linux, `proc_pidfdinfo` on macOS, no lsof | `--docker` runs `docker ps` | `doctor` for port conflicts, remote inspection over SSH, JSON output |
| [porthog](https://pypi.org/project/porthog/) | Python, CLI + TUI | not stated | no | dev-port ranges by default, framework detection, macOS and Linux |
| [PortPilot](https://dev.to/abdullahtarakji/building-portpilot-a-modern-tui-for-port-management-2p18) | Go, Bubble Tea | lsof on macOS, ss on Linux | no | list, check, kill, JSON; conflict detection |
| [killport-tui](https://github.com/last1chosen/killport-tui) | Rust, ratatui | sysinfo crate | no | filter, confirm-to-kill, Windows support |
| [somo](https://github.com/theopfr/somo) | Rust, table CLI | netstat replacement | no | interactive kill, JSON, recommends sudo |
| [osfacts](https://github.com/juspay/osfacts) | Rust, snapshot CLI | direct kernel calls, measured 24 ms host-wide | no | documents the macOS 27 PCB-list gating and Darwin permission edges |

Sources consulted for this specification, as of 2026-09-28: the pages linked above, the [Bubble Tea v2.0.0 release](https://github.com/charmbracelet/bubbletea/releases/tag/v2.0.0) and [v2.0.10 release](https://github.com/charmbracelet/bubbletea/releases/tag/v2.0.10), [gopsutil's darwin process source](https://github.com/shirou/gopsutil/blob/master/process/process_darwin.go) and [PR 1551](https://github.com/shirou/gopsutil/pull/1551) on its lsof-based connection lookup, and the [testing/synctest](https://pkg.go.dev/testing/synctest) package for the engine's timer tests.
