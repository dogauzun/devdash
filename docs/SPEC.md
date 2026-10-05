# devdash — Technical Specification

> The spec of record for devdash v1 (Release 0.1), for Release 1.0, the port answer, which adds to it (see [Release 1.0: the port answer](#release-10-the-port-answer)), and for Release 1.1, the port answer on screen (see [Release 1.1: the port answer on screen](#release-11-the-port-answer-on-screen)). Rulings made while building it are in [DECISIONS.md](../DECISIONS.md); `DEV-n` keys refer to the maintainer's private Jira tickets.

2026-09-28 · Doga · Status: v1

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

Non-goals for v1: Windows, remote hosts, a config file, log viewing, starting or restarting processes, Kubernetes, UDP and unix-domain sockets (not shown yet), notifications, and any daemon or background service. devdash runs only while its terminal is open.

## Architecture

The TUI and the CLI are thin clients of one engine; the engine owns the refresh loop, builds snapshots from the OS collector and Docker, and exposes the kill action; opening a port in the browser is the TUI's own (it reads only the selected row and changes nothing).

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
| `engine` | refresh loop, snapshot publishing, the kill action | collector, model, docker |
| `model` | pure functions: process tree, project resolution, kinds, Docker reconciliation, filter, row flattening | standard library only |
| `collector` | `Collector` interface, `linux` and `darwin` implementations, `fake` for tests | `golang.org/x/sys/unix`, `purego` (darwin) |
| `docker` | socket discovery, minimal Engine API client | `net/http` |
| `tui` | Bubble Tea v2 program, views, key handling | engine |

Threading: one goroutine runs the ticker (2 s by default) and calls the collector with a 1.5 s context timeout; the Docker client runs on its own 5 s cadence in a second goroutine. Each finished snapshot is published on a channel of size 1 where the latest value wins, and the TUI subscribes through a `tea.Cmd`; the UI goroutine never blocks on collection. Kill runs synchronously from the caller with a 3 s timeout and triggers an immediate refresh.

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
    Timing        Timing     // per-source durations, for --json only (the TUI does not show them)
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
    ContainerID string     // set when every listener this process holds is the same container's
    Unknown     FieldSet   // which fields could not be read (argv, cwd, cpu, mem, owner)
    Tags        TagSet     // Release 1.0: facts that suggest a leftover (orphaned, cwd_deleted)
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
    Name     string  // basename of Root, or the repository's name for linked worktrees
    Branch   string  // "" when detached (then ShortSHA is set) or unknown
    ShortSHA string
    Worktree bool
    MainRepo string  // main work tree, for linked worktrees only; "" when none is recorded
    Here     bool    // Release 1.0: the project of the directory devdash was run from
    CommonDir string // git common directory, the same for every worktree of one repository
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

**Linux.** The process list is the numeric entries of `/proc`. Per process: `/proc/[pid]/stat` gives ppid, state, utime, stime and starttime (clock ticks since boot at USER_HZ = 100, added to `btime` from `/proc/stat`); `/proc/[pid]/status` gives the effective uid (second field of `Uid:`), as macOS reports it; `/proc/[pid]/cmdline` is NUL-separated argv (empty for kernel threads, zombies, processes that blanked their argv, and briefly for a process in the middle of exec, whose comm is already the new program's; kernel threads, recognised by `PF_KTHREAD` in the stat flags, and zombies, state `Z` or `X`, are skipped, while a user process with an empty argv is kept); `/proc/[pid]/statm` gives resident pages; `readlink /proc/[pid]/cwd` fails with EACCES for other users' processes, and appends ` (deleted)` to a removed directory, which is stripped so both OSes report the old path. Listeners come from `/proc/net/tcp` and `/proc/net/tcp6`: rows with state `0A`, local address and port in hex (the address is printed as 32-bit words in host byte order), the socket's uid in the 8th column and its inode in the 10th (not the last: refcount, pointer and timer columns follow). A v4-mapped address in `tcp6` (`::ffff:127.0.0.1`) is reported as `tcp4 127.0.0.1`, as on macOS. Ownership is the inode: `readlink /proc/[pid]/fd/*` yields `socket:[inode]`, which is scanned in pid order (a socket shared across fork goes to the lowest pid) only for pids of our own uid (others fail anyway; all pids when running as root) and only while listeners whose socket uid is our own (all of them as root) remain unmatched. `/proc/net/*` is per network namespace, so sockets inside containers are not visible; their published ports appear as docker-proxy (or `dockerd` with the userland proxy disabled) and are reconciled through the Docker API instead.

**macOS.** The process list and its basic facets come from one `sysctl kern.proc.all` call (`unix.SysctlKinfoProcSlice` in `golang.org/x/sys/unix`): pid, ppid, uid, start time and the 16-character `p_comm`. Everything else comes from libproc, loaded with purego so the build stays `CGO_ENABLED=0`, which is how [gopsutil v4](https://github.com/shirou/gopsutil/blob/master/process/process_darwin.go) does it and where the struct layouts can be checked: `proc_pidinfo(PROC_PIDVNODEPATHINFO)` for cwd, `proc_pidinfo(PROC_PIDTASKINFO)` for CPU time and resident size, `proc_pidinfo(PROC_PIDLISTFDS)` then `proc_pidfdinfo(PROC_PIDFDSOCKETINFO)` per socket fd for listeners (kind `SOCKINFO_TCP`, state `TSI_S_LISTEN`, local port and family). argv comes from `sysctl kern.procargs2`. All of these work for our own uid without root.

For other users' listeners on macOS the only host-wide source is `sysctl net.inet.tcp.pcblist_n`, whose records carry a bind address, port, state and `so_last_pid`. One tool's README reports that on macOS 27 an ad-hoc-signed binary receives an empty PCB list while Apple-signed netstat still sees the listeners ([osfacts](https://github.com/juspay/osfacts)). The spike (macOS 27.0.1) found the gate follows process ancestry, not the caller's own signature: an ad-hoc-signed devdash started from a shell gets the full list, while any process with an ad-hoc-signed ancestor (`go run`, `go test`, and every child of an ad-hoc devdash, Apple-signed netstat and sysctl included) gets only the headers and its own sockets, with no error. The design does not depend on it: the per-uid fd walk is the primary source and an empty PCB list is treated as unknown rather than as no listeners. There is no netstat fallback: a `netstat -anv` spawned by an ad-hoc-signed devdash inherits the restriction and sees nothing.

Why not gopsutil for sockets: its darwin and FreeBSD connection lookups shell out to lsof ([gopsutil PR 1551](https://github.com/shirou/gopsutil/pull/1551)), which is slow on a busy machine and has broken on warnings written to stderr. gopsutil remains a reference for the darwin struct layouts and may be used for CPU and memory facets if that saves real time.

## Project resolution

A process belongs to the nearest git repository above its working directory; when that is unknown, the parent chain and argv are tried before giving up. The whole procedure is a pure function of the snapshot plus a small cache, so it is unit-tested with temporary repositories.

1. Take the process cwd. If it is unreadable, go to step 5.
2. Walk up from cwd until a directory contains a `.git` entry. A `.git` directory is a main repository. A `.git` file is a linked worktree when the git directory its `gitdir:` line names holds a `commondir` file, which names the repository's common directory (otherwise it is a submodule or a separate git dir, its own project). The main repository's work tree is the common directory's `core.worktree` when set (a submodule's), else the directory above a non-bare common directory called `.git`, else none: a bare repository has none, and git records none for a `--separate-git-dir`.
3. Stop at the filesystem root or at `$HOME`; a repository rooted at `$HOME` (dotfiles) does not count as a project, so the walk continues past it as if no `.git` were there.
4. The repository root is the project ID. Its name is the basename of the root, or of the main repository's work tree for a worktree, shown as `shop @ feat/cart (worktree)`; when there is no work tree, the basename of the common directory less a trailing `.git` (`api.git` gives `api`), or of its parent when that leaves a dot-directory or nothing (`shop/.bare` gives `shop`).
5. If no project was found, repeat steps 2 to 4 for the cwd of the parent, grandparent and great-grandparent (a daemon that changed directory after being started from a shell inside the repository).
6. Still nothing: take the first absolute path in argv that lies inside a project already found in this snapshot, and use that project. Paths that are not inside a known project are not walked, to avoid stat calls on arbitrary strings.
7. Otherwise the process goes under the `other` group, which is sorted last.

Branch comes from the repository's `HEAD` (`ref: refs/heads/<branch>`, or a 7-character SHA when detached), read from `.git/HEAD` for a main repository and from `<gitdir>/HEAD` for a worktree. A repository in git's reftable format keeps HEAD in its tables and writes the placeholder `ref: refs/heads/.invalid` to the file; devdash does not read reftables, so the branch of such a repository and its worktrees is unknown: empty, with no SHA, the header reads just the name, and the detail pane leaves out its branch field (DEV-163). Nested repositories and submodules resolve to the nearest `.git`, which is what a developer working inside the submodule expects.

Resolution results are cached per directory path with the mtime of the `.git` entry and of `HEAD`; a hit costs one `stat`, a miss costs the walk, and the cache is capped at 4096 entries. Every filesystem read uses `os.Lstat` and refuses to follow a symlink out of the walked path, so a process running in a symlinked directory still resolves to the real repository.

## Process tree and kinds

Within a project group, rows form a tree by ppid, and each process gets one kind from a small ordered rule table; the first matching rule wins.

The tree is built from the snapshot's pid → ppid map, restricted to processes in the same project. A process whose parent is outside the project (or is pid 1 or launchd after a re-parent) becomes a root of that group. Roots are sorted with listeners first, then by start time; children keep their parent's order. Kernel threads on Linux and processes with an empty argv are never rows. The dashboard draws a chain of processes that each have one child as one row (see "TUI design"); JSON and the CLI keep one entry per process.

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

A one-element argv holding spaces (a title rewritten with setproctitle, as Chromium's and Electron's children do on Linux, or a path with spaces run without arguments) names its program by its text up to the first space (without a trailing `:`, as in `postgres: checkpointer`), else by the whole string, only when that basename is the process name (or starts with a cut one); otherwise the process name is matched alone (DEV-168, DEV-175). The process name is matched whole, never by the text after a `/` in it, which only a title rewrite or a kernel thread puts there (DEV-175).

The lists live in one Go file with a comment per entry so contributors can extend them without touching logic. The default view hides `shell` and `editor`; a hidden process whose descendant is visible is still drawn, dimmed, so the tree stays connected. `a` in the TUI and `--all` in the CLI show everything.

## Docker integration

Docker is optional input: two GET requests over the engine's unix socket, a 500 ms timeout, and silence when the socket is absent.

Socket discovery, in order: `DOCKER_HOST` when set (unix and tcp URLs); the endpoint of the current docker context (`~/.docker/config.json` → `currentContext`, then that context's `meta.json`); then the first existing path among `~/.docker/run/docker.sock` (Docker Desktop), `~/.orbstack/run/docker.sock`, `~/.colima/default/docker.sock`, `/var/run/docker.sock`, `$XDG_RUNTIME_DIR/podman/podman.sock` and `~/.local/share/containers/podman/machine/podman.sock`. The chosen path is shown in the detail pane's footer so a wrong pick is visible.

The client is `net/http` with a custom `DialContext` on the socket, and the API version is negotiated rather than pinned: `GET /_ping` is sent unversioned, and `/containers/json` is then requested under the version in its `Api-Version` response header (`/v1.44/containers/json`), or unversioned when that header is absent or not of the form `1.<digits>`, so that Docker 20.10 and newer (Docker 29 refuses API versions below 1.44) and Podman's compatibility API all answer. `GET /_ping` runs once at start and after each failure, and each ping re-reads the version; `GET /containers/json` runs every 5 s on its own goroutine. Only these fields are read: `Id`, `Names`, `Image`, `State`, `Labels["com.docker.compose.project"]`, `Labels["com.docker.compose.service"]`, and `Ports[].{IP, PrivatePort, PublicPort, Type}`. The moby client library is not imported; it would multiply the dependency tree for two endpoints.

Reconciliation runs in `model` after every collector snapshot: a listener matches a published tcp mapping when the ports are equal, the listener's bind address equals the mapping's host IP or, failing that, is unspecified (an unspecified listener prefers a mapping on every interface; an empty host IP, as Podman reports it, means every interface), and either the listener's process name is one of `docker-proxy`, `dockerd` (Docker Engine 28+ with `--userland-proxy=false` holds each published port itself), `com.docker.backend`, `com.docker.vpnkit`, `vpnkit`, `gvproxy`, `rootlesskit`, `rootlessport`, `slirp4netns`, `pasta`, `limactl`, `OrbStack Helper` (OrbStack on macOS: one process, running as the user, holds every published port), or the listener's owner is unknown (root-owned docker-proxy on Linux). A matched listener gets `ContainerID` and its process kind `container`. The process gets `ContainerID` only when every one of its listeners is the same container's: it is then displayed with the container name and image instead of the proxy's name, under the container's compose project, or under `containers` when it has no compose label. Docker Desktop's `com.docker.backend` (like `OrbStack Helper`) holds every container's ports in one process, so when it holds two or more containers' ports, or one container's next to a port of its own (`OrbStack Helper`'s 32222, Docker Desktop's Kubernetes on 6443), it keeps its own row with kind `container` and each container gets a container row, whose detail pane names it (`held by OrbStack Helper 12151 (forwards the container's port)`, as `port N` says `via OrbStack Helper`, DEV-180); only the forwarded listeners carry the container. With the userland proxy disabled, Docker Engine 28+ keeps the listening socket in `dockerd`, which reconciles like `docker-proxy` when devdash can see its pid (root) and as an unknown owner otherwise; a published port with no socket on the host (iptables only, as older engines do with the userland proxy disabled) still appears as a container row with no process behind it, and its detail pane says `no process holds the port (published by Docker)`; when a forwarder holds some of the container's published tcp ports but not all, the pane names each of the others after the `held by` lines, once per port: `no process holds port 6001 (published by Docker)` (DEV-186).

Failure handling: a missing socket produces no warning at all; a socket that exists but does not answer, or answers a request slower than 500 ms, keeps the previous container list and produces one footer hint ("docker: not reachable at <endpoint>") and a retry every 10th tick. After a failure or a missing socket no request is made until 10 refresh ticks (10 × `--tick`) have passed since the fetch that failed began; the retry is the first 5 s fetch at or after that point, so the wait is 10 ticks rounded up to whole 5 s beats (20 s by default, 15 s at `--tick 1.1s`). That fetch pings and lists, and counts when it starts up to 1 s early, so scheduling jitter on the 5 s beat does not push the retry to the next beat. This is also how a socket that appears later at the same path is found. A socket this user may not open (EACCES or EPERM on the dial; on Linux `/var/run/docker.sock` is `root:docker` 0660) is handled the same way under the same `docker_unreachable` code, with the hint "docker: permission denied on <path> (add yourself to the docker group)" on Linux and "docker: permission denied on <path> (owned by another user?)" on macOS (Docker Desktop and OrbStack sockets live in the user's home and belong to the user; there is no docker group to join), so it does not read as Docker being down. Without a container list, a published port shows up as an unknown owner or a runtime process, so `port N` appends the Docker hint to such a line ("owner unknown: run with sudo to see owners; docker: permission denied on …"), and `kill N` to its refusal. When discovery finds no endpoint at all, the dashboard runs it again on the same cadence, silently: at its first fetch and then on the first 5 s fetch at or after each 10 ticks, until it finds one, which it then keeps and asks as above (the detail pane's footer shows it from then on). So Docker or Podman started after devdash, whose socket did not exist yet (dockerd removes `/var/run/docker.sock` on a clean stop), shows up within one retry. A docker context endpoint that a later discovery finds but cannot use (a context switched to a TLS host) gives the same `docker_endpoint_invalid` warning as at start. `port`, `kill` and `--json` discover once.

## Actions

Every destructive action shows what it will do first, refuses a fixed set of targets, and reports what survived.

**Kill.** Three modes, chosen in the confirmation modal (`p`, `t`, `f`) or by CLI flags:

- process: `SIGTERM` to the selected pid.
- tree: `SIGTERM` to the selected pid and all its descendants, parent first so a supervisor such as nodemon or `air` cannot respawn a child that was killed before it. When the selected process is a process-group leader (`pgid == pid`), the signal is also sent to `-pgid` so group members outside the visible tree (a shell's job) are covered.
- force: the same set with `SIGKILL`.

After signalling, the engine polls every 100 ms for up to 3 s (`--timeout`), then reports survivors by pid and name; the TUI offers force on the survivors. Descendants are computed from the latest snapshot's ppid map at the moment of the action, and pids are re-validated against their start time before each `kill(2)` so a recycled pid is never signalled.

**Refused targets.** pid 0, pid 1 (`init`, `launchd`), devdash's own pid, and every ancestor of devdash (the shell and terminal running it). A row that represents a container port is refused with the hint `docker stop <name>`. A process of the container runtime (`dockerd`, `containerd` and its shims, `docker-proxy`, `com.docker.backend`, `com.docker.vpnkit`, `vpnkit`, `gvproxy`, `rootlesskit`, `rootlessport`, `slirp4netns`, `limactl`, `pasta`, `conmon`, `OrbStack Helper` and the `OrbStack` app; names are matched case-insensitively on the basename of the process name and of argv[0], read as the kinds read it, and the list lives next to the kind lists) is refused by name, whether or not Docker answered, with a hint to find the container: `docker ps --filter publish=<port>` when it is Docker's and holds one port, otherwise `docker ps`, `podman ps` or both, by runtime. Killing it stops other containers or leaves a published port dead. A process owned by another user is attempted, and the resulting `EPERM` is shown as "permission denied, run with sudo". Killing a process outside any project group asks for a second confirmation, because those are usually system services.

**Confirmation.** The modal lists every pid that will receive the signal with its name, project and ports, and the mode. Its title is one line, `kill <label> (pid N): <mode>, <SIGNAL> to N processes` (and likewise when refused, running or reporting); when it is wider than the screen the label is cut with `…`, by display width, so the pid, mode, signal and count always show, down to a label of 6 cells, past which the line is cut at the edge (DEV-184). `--yes` skips it on the CLI only, together with the second confirmation for a target outside every project; the TUI always confirms. Without `--yes` the CLI asks on the terminal and, when stdin is not a terminal, exits 2 without signalling.

**`kill N` on the CLI.** N is a TCP port. Every distinct owner of a listener on it gets its own plan; the plans are shown together and confirmed once, and if any owner is refused (including an unknown PID 0 owner), nothing is signalled. After the wait the port is checked again in a fresh snapshot: a forked child can still hold a socket credited only to its parent, so the CLI names the holder and suggests `--tree`; the exit code still describes only the processes that were signalled.

**Rerun under sudo.** devdash works without root; elevation is opt-in and never automatic. In the dashboard's table, with no modal, overlay or prompt open, `S` opens a confirmation saying that the dashboard restarts as root and that the selection, filter and sort are reset; `y` confirms and any other key cancels. In a short terminal the explanation gives way before the line naming those keys, which is the last line dropped (DEV-184). On confirm the dashboard quits normally (terminal restored, refresh loop stopped), then devdash replaces itself (`execve`) with `sudo -- <its own executable> <the arguments it was started with>`, in the current environment; sudo asks for the password on the terminal and devdash never reads it. A dashboard that ends because of a signal (SIGINT, SIGTERM or SIGHUP) never reruns, even when the signal arrives after `y`. If sudo cannot be started, the error goes to stderr and devdash exits 5. `S` is offered, and named in the footer's key hints and in a kill result, only when devdash is not root, `sudo` is on `PATH`, and either the snapshot has a warning that root would not have (`process_fields_unreadable`, or `listener_owner_unreadable` when an owner may be hidden by permissions; on Linux only when `CAP_SYS_PTRACE` is in the bounding set, so the root sudo starts can read other users' processes) or a kill ended with permission denied. Otherwise `S` does nothing. It is not offered for Docker's permission warning, for a listener outside devdash's pid namespace or held by the kernel, nor as root. `--json`, `port N`, `kill N` and `free N` never rerun anything; they keep their text hints.

**Open in browser.** `o` runs `open` on macOS or `xdg-open` on Linux with `http://localhost:<port>`, using the lowest port when the process has several. No HTTPS detection in v1; a wrong scheme costs the user one click. Under sudo the opener runs as the invoking user, never as root: when devdash's effective uid is 0, `SUDO_UID` and `SUDO_GID` (decimal, uid not 0) give its uid and gid, with no supplementary groups, and its `HOME`, `USER` and `LOGNAME` come from that user's entry in the user database (on Linux also `XDG_RUNTIME_DIR=/run/user/<uid>` when sudo removed it). Root without a valid `SUDO_UID` and `SUDO_GID` (a root login, a container) or whose user cannot be looked up starts nothing, and the footer says `open failed: not available as root`.

## CLI and JSON schema

The CLI exposes the same snapshot the TUI shows, and its exit codes are the contract scripts rely on.

| Command | Output | Exit code |
| --- | --- | --- |
| `devdash` | the TUI; without a terminal on stdout (piped, redirected, cron, `ssh` without `-t`) nothing starts and stderr gets `devdash: the dashboard needs a terminal; use --json, port N or free N in scripts` | 0 quit (`q`, `ctrl-c`, SIGINT, SIGTERM or SIGHUP), 2 no terminal on stdout, 5 devdash failed |
| `devdash --json` | one snapshot as a JSON document on stdout | 0; 5 devdash failed |
| `devdash port 3000` | owner line(s): pid, name, project, bind address; or `free` | 0 found, 1 free, 5 devdash failed |
| `devdash port 3000 --json` | Release 1.0: the same answer as one JSON object | as `port 3000` |
| `devdash free 3000` | Release 1.0: the first free port from 3000 to 3099, or nothing | 0 found, 1 none free in range, 2 usage error, 5 devdash failed |
| `devdash kill 3000 [--tree] [--force] [--yes] [--timeout 3s]` | the plan (mode, signal, every pid with name, project and ports), then what was signalled, survivors, and whether the port is free | 0 all exited (or nothing listens on the port), 2 usage error or no terminal to confirm on without `--yes`, 3 permission denied (or the owner is unknown), 4 survivors remain, 5 devdash failed before anything was signalled, 6 nothing signalled: a refused target or a declined confirmation |
| `devdash version` | version, commit, commit date (labelled `built`; a `go install …@version` from the module proxy has the version only: `commit none, built unknown`) | 0 |

Global flags: `--roots <paths>` limits project scanning to repositories under those directories (comma-separated, and repeatable; a leading `~` is `$HOME`, `~user` is not supported, and a path that is not an existing directory is a usage error); `--tick <duration>` sets the refresh interval (default 2s, minimum 500ms; a smaller value is a usage error); `--no-docker` skips the Docker client; `--all` includes shells and editors in the TUI (`--json` always lists every process); `--no-color` and `NO_COLOR` disable colour. Flags may come before or after the subcommand. Usage errors exit 2 and print to stderr; stdout stays clean for `--json`. `-h` prints the usage on stdout and exits 0. Every command exits 5 when devdash itself fails (no snapshot could be taken, or the output could not be written), with the error on stderr and nothing on stdout; `kill` only before anything was signalled, since after that its code reports the signals; 1 only ever answers the question asked (`port`: the port is free; `free`: no port in range is free) and never means devdash failed.

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

**Table.** Columns in priority order: name (indented by tree depth, container rows carry the container name and image), kind, ports (comma-joined, with a leading `*` when bound to every interface), pid, uptime, cpu, mem, user, command (argv joined with single spaces, an absolute argv[0] by its basename, truncated to the remaining width; the detail pane shows the full path, DEV-146). Below 100 columns cpu, mem and user are dropped; below 90 the command is dropped. Project headers show `name @ branch (worktree)`, the process count and the port count, and collapse with `←`. Groups are ordered by most recent activity (latest start time in the group), `containers` and `other` last.

A chain of processes P1 → … → Pn (n ≥ 2) in one group, where every process before the last has exactly one shown child, the next, is one row (DEV-157): `bash › claude › bash › xargs`, the labels joined by ` › `, at P1's depth, with Pn's children one level below it. A process before Pn that the view hides (a shell or an editor without a listener, unless `a`) is left out of the label, so that chain reads `claude › xargs` and `zsh › vite (node)` reads `vite (node)`; while a filter is set, such a process that matches it is drawn, so `/zsh` shows `zsh › vite (node)`. Pn's own label always stays, hidden kind or not (`disclaimer › claude › zsh` above zsh's children), and `→` unfolds a folded row even when its label is one name (DEV-160). The row is Pn's: kind, ports, pid, uptime, cpu, mem, user, command, tags, arguments, detail pane, `o` and `x` are the last process's (`x` in process mode plans for Pn alone, as on Pn's own row; tree mode plans from the first process the label draws, so `nodemon (node) › server.js (node)` signals nodemon and its server together, and a link the label leaves out is never the root (DEV-179)), it is dimmed only when Pn is, and it sorts among its siblings by Pn, or in name mode by the label the view draws without a filter. A process with a listener or a tag, a container's process, a collapsed row and the unknown owner can end a chain but never sit inside one, so no port or tag leaves the table. When the label does not fit the name column, leading drawn links give way to `… › ` and the last label is always kept. The filter matches a folded row when any of its processes would match as its own row. A search flattens with every row shown, so a chain can end in a process the view hides; when that process neither matches nor leads to a match, the row is cut back to its last process the view shows or that matches, drawn as that process (`claude` for claude and its idle shell), or left out when there is none. Group headers count processes, not rows.

**Detail pane.** `enter` opens it as a right split at 120 columns or more, otherwise as a full-screen overlay: full argv (wrapped), cwd, project and branch, listeners with bind address, parent chain up to the root, start time, user, and the Docker socket in use when the row is a container.

| Key | Action |
| --- | --- |
| `↑` `↓` `j` `k` | move the selection |
| `←` `→` `h` `l` | collapse or expand a project group or a tree node; `→` on a folded chain unfolds it, `←` on its first row folds it again |
| `enter` | open or close the detail pane |
| `/` | filter by port, name, argv, project, container or tag (Release 1.0); `esc` clears |
| `0`-`9` | port search: opens the filter with the digit typed and selects the port's holder (Release 1.1) |
| `x` | kill modal: `p` process, `t` tree, `f` force, `esc` cancel |
| `o` | open `http://localhost:<port>` |
| `a` | show or hide shells and editors |
| `d` | show or hide container rows |
| `s` | cycle sort within groups: default, port, cpu, start time, name |
| `r` | refresh now |
| `S` | rerun the dashboard under sudo, after `y` confirms; offered only when sudo would help (Actions, Rerun under sudo) |
| `?` | help overlay |
| `q` `ctrl-c` | quit |

**Refresh and selection.** The model keeps the latest snapshot and a flattened row list computed from it. Selection is stored as a row key, `(pid, start_time)` for a process or the project ID for a header, never as an index; after a new snapshot the key is looked up again (a process folded into a chain's row finds that row), and if it is gone the selection moves to the row that now occupies the nearest previous index. Expansion state is a map keyed the same way and survives refreshes, and so is the set of unfolded chains, keyed by their first process. The filter runs on the rows flattened as if nothing were collapsed, so a match inside a collapsed group or under a collapsed tree node is found, and keeps the ancestors of every match visible so the tree never shows a child without its parent. The expansion state is kept and applies again when the filter is cleared; while a filter is set, `←` only moves to the parent row and `→` does nothing. All of this is pure and tested by sending messages to the model and asserting on `View()`.

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

Rules for the loop: collection runs with a context timeout of 1.5 s; a timed-out tick keeps the last good snapshot and the header shows `stale N s`. When three consecutive ticks exceed 500 ms the interval doubles, up to 10 s, and a footer hint says so; it halves back when ticks are fast again. CPU percent is the delta of process CPU time over the delta of wall time between two samples of the same `(pid, start_time)`, shown as `–` on the first sample: the previous sample, or, for a tick a refresh started less than `--tick` after it, the sample that one was measured from, so the window is never shorter than a tick (DEV-181). On Linux the fd walk skips pids whose `/proc/[pid]/fd` failed with EACCES in the previous tick unless the set of unmatched listener inodes changed. The snapshot is built into fresh slices each tick; nothing is mutated in place, so the UI can hold the previous snapshot without locks.

| Condition | Behaviour |
| --- | --- |
| cwd, argv, fd or task info unreadable (other uid) | row shown, fields marked unknown, one warning with a count, footer hint `run with sudo` (as Linux root, which sudo cannot help: names the missing CAP_SYS_PTRACE, `--cap-add SYS_PTRACE`; with it, names a security module or sandbox; an unowned listener when no fd read was denied is outside devdash's pid namespace or held by the kernel, and the hint says that); where sudo would show the fields or owners, the dashboard also offers `S` to rerun itself under sudo (Actions) |
| macOS PCB list empty or denied | other users' listeners treated as unknown (own-uid listeners still come from the fd walk); one warning; hint in footer; no netstat fallback |
| Docker socket absent | no Docker rows, no warning; the dashboard looks for it again every 10th tick (discovery again when none was found at start) |
| Docker socket present but unreachable or slow | previous container list kept, one footer hint `docker: not reachable at <endpoint>`, retry every 10th tick (the first 5 s fetch at or after 10 × `--tick`, 20 s by default) |
| Docker socket present but access denied (EACCES/EPERM: on Linux, a user outside the `docker` group; on macOS, another user's socket) | as unreachable, with the hint `docker: permission denied on <path> (add yourself to the docker group)` on Linux, `docker: permission denied on <path> (owned by another user?)` on macOS; `port N` and `kill N` append it to an unknown owner's or runtime process's line |
| process exits during collection | skipped silently; a partially read process is dropped rather than shown half-filled |
| more than 5000 processes | interval starts at 5 s; the dashboard reads argv only for processes in a project or with a listener, plus those whose kernel name may be cut (15 bytes or more) or may be a container runtime's, so `kill` still refuses a runtime by name, and those named `systemd`, so the `orphaned` tag still finds a `systemd --user` parent; every other row has its argv unknown. `--json`, `port N` and `kill N` read every argv |
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
- Release: goreleaser builds archives (the binary, `LICENSE`, `THIRD_PARTY_LICENSES`, `README.md` and the three docs it links to for usage: `docs/usage.md`, `docs/limitations.md`, `docs/json-schema.md`; every entry owned by root with the commit time), checksums and a changelog from conventional commits, publishes a GitHub release, and updates a Homebrew cask in a `homebrew-tap` repository (a cask rather than a formula since goreleaser v2.16 deprecated formulas for prebuilt binaries; casks install on macOS and Linux). `go install github.com/<owner>/devdash/cmd/devdash@latest` works from the first tag.
- Versioning: semver. The port-answer release ships as 1.0.0 (Doga, 2026-10-03). From 1.0.0, JSON schema v1, the CLI exit codes and the keybindings are a compatibility promise: fields, commands and keys are added, never removed or renamed, without a new major version.
- Signing: Go's linker ad-hoc signs darwin/arm64 binaries, which is enough to run; notarization is not needed for a CLI. macOS 27 withholds the PCB list from any process with an ad-hoc-signed ancestor, and devdash's own signature does not matter when it is launched from a shell. So the per-uid fd walk stays the primary source. The PCB list adds other users' listeners when the kernel serves it, an empty list is "unknown" with a footer hint, and there is no netstat fallback.
- Repository contents on day one: `README.md` (install, keybindings, a short "why another port tool" comparison, demo GIF), `CLAUDE.md` (build, test and lint commands, conventions), `DECISIONS.md` (one line per non-obvious choice), `demo.tape` for [vhs](https://github.com/charmbracelet/vhs), `.goreleaser.yaml`, `.golangci.yml`, `LICENSE` (MIT).

## Release 1.0: the port answer

Release 1.0 makes `devdash port N` the place to decide what to do about a busy port, and gives both outcomes, killing the holder or moving to another port, one step each. The usual moment is a dev server failing with "address already in use": the developer asks what holds the port, then either kills it or starts on another port. v1 answers only who (`15669  python3  shop  0.0.0.0:5173`), which hides the real command and says nothing about whether the holder is the developer's own leftover. Only devdash knows the repository, worktree and branch of a process, so Release 1.0 uses that to help answer "should I kill it", and adds "which port can I use instead".

Release 1.0 is done when these four goals hold on both operating systems without root, on top of the eight v1 goals:

- `devdash port N` shows, for each holder, the full command, the project with branch or worktree, the uptime, and whether it is in the repository devdash was run from.
- A holder carries a tag when a fact suggests it was left over: `orphaned` (its parent exited) or `cwd deleted` (its working directory is gone). Tags state facts; devdash never says a process is safe to kill.
- `devdash free N` prints the first port at or above N that nothing listens on and that the OS lets this user bind, so `PORT=$(devdash free 3000) npm run dev` works.
- Scripts written against v0.1 keep working: piped `port N` output is unchanged, and JSON only gains fields.

Non-goals for Release 1.0: an idle tag (a dev server without traffic is idle and fine), killing a whole project at once, a separate "left running" view, jumping to the terminal a process runs in, and reserving a port. UDP and unix-domain sockets are not shown yet.

**Here.** At startup devdash resolves its own working directory with steps 1 to 4 of project resolution. The project found, if any, has `Here` set in every snapshot; when devdash runs outside any repository, no project has it. A process is in `this repo` when its project is the `Here` project, and in `this repo, other worktree` when the two projects differ but share a repository: the same git common directory (`CommonDir`), which also covers the worktrees of a bare repository.

**Tags.** A tag is computed per snapshot. `orphaned` uses data the collector already reads; `cwd deleted` needs one new read that the collector records per process: an `lstat` of the cwd for every process whose cwd is known on macOS, and on Linux a `stat` of `/proc/<pid>/cwd` only for a process whose cwd link carried the ` (deleted)` suffix. Rows with PID 0 and container rows never carry tags.

| Tag | Linux | macOS | Wrong when |
| --- | --- | --- | --- |
| `orphaned` | ppid is 1, or the parent is a `systemd --user` subreaper (name `systemd`, same uid, `--user` in argv) | ppid is 1 (launchd) | a server daemonized on purpose (a `--daemon` flag) is tagged too |
| `cwd deleted` | the kernel's ` (deleted)` suffix on `/proc/<pid>/cwd` (which the collector strips from `Cwd`), confirmed by a `stat` of `/proc/<pid>/cwd` showing a link count of 0, so a live directory really named `x (deleted)` (DEV-44) is not tagged; the `stat` runs only when the suffix is present | `lstat` of the cwd from `PROC_PIDVNODEPATHINFO` fails with `ENOENT` | macOS: the directory was recreated at the same path, so the tag is missed; on both systems it is never invented |

`orphaned` is set only on a process that belongs to a project or carries `cwd deleted`. System services and user-session agents (sshd, cron, pipewire, every macOS launch agent) also have init, launchd or a subreaper as their parent, and tagging them would bury the signal. A process whose worktree was removed often resolves to no project, because the walk starts from a directory that no longer exists; its port answer shows the old cwd instead of a project.

**The port answer.** When stdout is a terminal, each process holder gets up to three lines after its last v1 line (once per process, even when it holds N on two sockets), indented by seven spaces:

```console
$ devdash port 5173
15669  python3  shop  0.0.0.0:5173
       uvicorn app:main --reload --port 5173
       shop @ feat/login (worktree), up 3h, this repo
       orphaned, cwd deleted
next free: 5174
```

1. The command: argv joined with single spaces, an absolute argv[0] by its basename unless argv is one string holding spaces (a title rewritten with setproctitle) (a macOS framework Python runs as `/Library/…/Python.app/Contents/MacOS/Python`, over 100 cells; JSON keeps the path), cut to the terminal width with a trailing `…`. Omitted when argv is unknown.
2. Where: the project as `name @ branch (worktree)` (the TUI's group header label, without the `(here)` suffix, which the location marker replaces), cut from the right with a trailing `…` when the line is wider than the terminal, before the ` (worktree)` suffix, which stays while the name and branch keep at least one cell (`shop @ feature/very-long-br… (worktree)`, DEV-178), down to a lone `…`; the uptime and marker are never cut, so a terminal narrower than them still wraps the line (DEV-171), or the cwd when the process has no project, cut from the left with a leading `…` so that the line fits the terminal width (DEV-146), or `-` when that is unknown too; then the uptime in the TUI's format; then the location marker (`this repo` or `this repo, other worktree`) when one applies.
3. Tags, comma-separated in the table's order, only when at least one applies.

Container lines and the PID 0 unknown-owner line keep their v1 form and get no extra lines. Whenever `port N` exits 0 (a listener on N, or a container publishing N with no socket) and N is below 65535, the answer ends with `next free: <port>` from the same search as `devdash free N+1`, or `next free: none in <N+1>-<min(N+100, 65535)>`; for N = 65535 there is no `next free` line. When the probe behind that line fails (see Free port), the answer stays: the `next free` line is left out, `devdash: next free: <error>` goes to stderr, and the exit code is unchanged. When stdout is not a terminal, the output is byte-identical to v0.1.1. Exit codes do not change.

`devdash port N --json` prints one object: `schema_version`, `taken_at`, `port`, `free` (boolean), `holders` (process objects exactly as in the snapshot's `processes`, the PID 0 unknown owner included), `containers` (container objects of containers publishing N), and `next_free` (a number, or `null` when the port is free, when N is 65535, when nothing in range is, or when the probe failed, which also prints `devdash: next free: <error>` on stderr). Like `--json`, it samples twice, 200 ms apart, so the holders are exactly the process objects `--json` would print (`cpu_percent` a number) and `taken_at` means what it means there; plain `port N` keeps its single sample. It is the one command `--json` combines with; every other command with `--json` stays a usage error. Exit codes are those of `port N`.

**Free port.** `devdash free N` (1 ≤ N ≤ 65535) takes one snapshot and tries the ports N, N+1, … up to N+99 or 65535, whichever comes first. It prints the first port that passes both checks and exits 0, or prints nothing and exits 1 when none does.

1. Nothing in the snapshot holds it: no listener on any address, and no container publishes it on any host address. A port published only by iptables (Docker without its userland proxy) has no socket, so the bind below would not see it.
2. This user can bind it: devdash creates a TCP socket with `golang.org/x/sys/unix`, sets `SO_REUSEADDR` on Linux only (where it allows a port in TIME_WAIT, as a dev server's own bind would, and still fails beside any listener), binds `0.0.0.0:P`, then an `IPV6_V6ONLY` socket on `[::]:P`, and closes both at once. Go's `net.Listen` is not used because it sets `SO_REUSEADDR` everywhere, and on macOS that lets a wildcard bind succeed beside another socket's specific-address bind. The bind catches listeners of other users that a snapshot without root cannot see. A failed IPv6 socket or bind with `EAFNOSUPPORT` or `EADDRNOTAVAIL` (no IPv6 on the host) skips the IPv6 check. A bind that fails with `EADDRINUSE` or `EACCES` (below 1024) means the port is not free. Any other failure of `socket`, `setsockopt` or `bind` (`EMFILE`, `ENFILE`, `EPERM` under a sandbox) means devdash could not look: `free` exits 5 with the error on stderr and nothing on stdout, so exit 1 never stands in for a failed probe. `port N` keeps its answer instead, because the holders it found are still the answer: it leaves out the `next free` line (`--json`: `next_free` is `null`), prints `devdash: next free: <error>` on stderr and keeps its exit code.

On macOS a port in TIME_WAIT counts as taken, since the probe there runs without `SO_REUSEADDR`; the answer errs on the safe side. The answer means free at the moment of the check, not reserved: another process can take the port before the developer's server starts, and the README says so.

**TUI.** The dashboard shows the same facts with no new keys. The `Here` project's header ends in `(here)` and is sorted first, ahead of the activity order. A row with tags shows them dimmed after its name (`python3  orphaned`), shortened to one `!` below 90 columns. The detail pane lists each tag with what it means ("parent exited; now a child of launchd", "working directory deleted"). The `/` filter also matches tags, written either way (`cwd deleted` or `cwd_deleted`), so `/orphaned` lists every orphaned process.

**JSON.** Schema v1 gains `processes[].tags` (an array of `"orphaned"` and `"cwd_deleted"`, empty when none applies) and `projects[].here` (a boolean). Both are always present, so `schema_version` stays 1.

## Release 1.1: the port answer on screen

Release 1.1 makes the dashboard answer "what is using this port" as well as `devdash port N` does, because that is the question the dashboard is most often opened with. The flow becomes: type the port, read the answer, then press `x` to kill the holder or read `next free` to move. Everything is inside the TUI: JSON, `port N`, `free N` and `kill N` do not change.

Release 1.1 is done when these goals hold on both operating systems without root, on top of the v1 and Release 1.0 goals:

- Typing a port number in the table finds its holder wherever it is (a collapsed group, a hidden shell's child) and selects it, so `5173` then `x` kills it.
- While the search is a port number, one line says how many holders it has (processes and containers) and which port is free next, from the same search as `devdash free`, or that it is free.
- A row run by an interpreter names the tool (`vite (node)`), and at 80 columns the arguments fill the name column's spare width.
- The detail pane says whether a process is in this repo and which port is free next, and the kill result says whether the killed processes' ports are free.

Non-goals for Release 1.1, considered in the design and parked: mouse support (capturing the mouse breaks the terminal's own text selection), copy to clipboard (OSC 52 is ignored by macOS Terminal.app), a separate ports view, killing a whole project, rows that linger after they exit, a colour per kind, and `devdash 5173` opening the dashboard on that search. UDP and unix-domain sockets stay out of it.

**Search.** While a filter is set, the rows are flattened as if nothing were collapsed (Release 1.0, DEV-126) and with every row the view toggles hide shown (shells and editors without `a`, container rows with `d`), then filtered, so `/zsh` and `/nvim` find their rows without `a`, and `8000` finds a container with `d` on. A row the view hides that matches the query itself is drawn normally; one kept only as the ancestor of a match is dimmed, as hidden-but-connected rows are; one that neither matches nor leads to a match is left out, so a query that matches a group header (`/shop`) shows that group as the view shows it, without its idle shells. A group left with no row is left out. With `a` on and `d` off, every row is drawn as the view draws it.

**Port search.** In the table, with no modal, overlay or prompt open, a digit key opens the filter prompt with that digit typed, exactly as `/` and then the digit would; the detail pane may be open. Digits were unbound, so no key changes meaning. A query is a port number when it is digits only, without a leading zero, from 1 to 65535. Each time the query changes to a port number and a row holds exactly that port (a listener on it, or a container publishing it), the first such row in display order is selected. Prefix matches still show (`80` lists 8000 and 8080), but the exact holder wins the selection. Every other query, and every refresh, keeps the selection rules of "Refresh and selection". So the port question is `5173` `enter`, then `x` to kill or `enter` to read the detail pane.

**The port line.** While the query, typed or applied, is a port number N, a line under the header answers it. Its holders are counted as `port N` and `kill N` count them: each process with a listener on N, the unknown owner included, and each container publishing N, once, however many processes forward its port (Linux's userland proxy runs one `docker-proxy` per address family). A search for N selects the first row, with everything shown (shells, editors and containers, nothing collapsed), that stands for one of them: a process with a listener on N, or a row drawn as a container publishing N. A listener that is a container's published port holds N on the process's row only when that row is drawn as the container; a forwarder that keeps its own row (`OrbStack Helper`, Docker Desktop's `com.docker.backend`) leaves the port to the container's row. `next free` is the search `devdash free N+1` runs (`freeport.Find` over the current snapshot with the same bind probe), and the range it names is N+1 to min(N+100, 65535).

| Situation | Port line |
| --- | --- |
| N has K holders | `port 5173 · 1 holder · next free 5174` |
| N has K holders, nothing in range is free | `port 5173 · 1 holder · no free port in 5174-5273` |
| N has no holder and N binds | `port 3000 · free` |
| N has no holder but the bind fails | `port 3000 · next free 3001 · bind refused` |
| the probe failed (`EMFILE`, a sandbox's `EPERM`) | `port 5173 · 1 holder · next free: <error>`, the error part in the warning colour |
| N has no holder and the probe of N itself failed | `port 3000 · probe failed: <error>`, the error part in the warning colour |
| N = 65535 | `port 65535 · 1 holder`, or `free`, `bind refused` or `probe failed: <error>`, with no `next free` |

`bind refused` means a listener devdash cannot see holds the port (another user's on macOS, one in another network namespace) or this user may not bind it (below 1024 on Linux); with no holder of N and the bind failing, the parts are `next free …` (or `no free port in …`) and then `bind refused`. Until the first answer for the current query arrives, the line shows only what the snapshot says: `port 5173 · 1 holder`, or `port 3000` when nothing holds it. The probe runs in a `tea.Cmd`, off the UI goroutine, when the query changes to a port number and on each new snapshot while it is one; an answer for an older query or snapshot is dropped. It binds at most 101 ports, never runs on the refresh path and shells out to nothing. The probe is `tui.Options.Probe` (`freeport.Probe` when nil), so TUI tests never bind a socket. The line is cut with `…` at the screen edge, which is why `next free` comes before `bind refused`. While the line is shown, the table under it never says `nothing to show`: the line is the answer.

```text
mbp · 2 s ago · 2 projects · 6 listeners · 2 containers · /5173_
port 5173 · 1 holder · next free 5174
NAME                                          KIND      PORTS           PID   UP
▾ shop @ feat/cart (worktree) · 4 processes · 1 port
>   vite (node)  --port 5173                  server    *5173           101   3h
```

That screen is what the user gets even when the shop group was collapsed (`>` marks the selected row, drawn in reverse video).

**Tool labels.** A process whose argv[0] is an interpreter (the `interpreters` list next to the kind lists: node, nodejs, bun, bunx, npx, python of any version, ruby) and that runs a tool is labelled `<tool> (<name>)`, where the tool is the argument `Classify` unwraps (the first non-flag argument, or the module after `-m`, which python also takes attached as `-mMODULE`; bun's subcommands `run` and `x` are skipped, so `bun run dev` is `dev (bun)`) as written, by its basename with its extension kept, and name is the process name the row showed before: `vite (node)`, `uvicorn (python3)`, `pytest (python3)`, `server.js (node)`, `manage.py (python3)`, `vitest (npx)`. When that name is the script's own (Linux names a `#!` script's process after the script, cut to 15 bytes: `manage.py` for `#!/usr/bin/python3` run as `./manage.py`), name is the basename of argv[0] instead, so `manage.py (python3)` on Linux where macOS reads `manage.py (Python)` (DEV-169). A process with no tool keeps its name; inline code is not a tool, so an interpreter given its inline-code flag before any tool (python's `-c`; node's and bun's `-e`, `--eval`, `-p`, `--print`; ruby's `-e`; npx's `-c` and `--call`, a command string: `python3 -c "from multiprocessing.spawn import …"`, `node -e …`) keeps its name too, and python's `-X` and `-W`, node's (and nodejs's) `-r`, `--require`, `--import`, `--env-file` and `--env-file-if-exists`, bun's `-r`, `--preload`, `--require` and `--import`, npx's and bunx's `-p` and `--package`, and ruby's `-I` and `-r` take the next argument as their value, so `node -r ./register.js app.js` is `app.js (node)` and `npx -p @angular/cli ng serve` is `ng (npx)` (DEV-137, DEV-138, DEV-153, DEV-170, DEV-176). A shell (sh, bash, zsh, dash, ksh, fish) running a script file is labelled the same way, `run55.sh (bash)`, after the basename of argv[0] rather than the process name, so that it is the same on Linux, where a `#!` script's process is named after the script, and keeps its kind `shell`; a shell given inline code or standard input (a cluster of short flags holding `c` or `s`: `sh -c …`, `bash -lc …`, `sh -s`; fish's `--command`) or no script (a login or interactive shell) keeps its name, and `-o`, `-O`, `+o`, `+O` (also ending a cluster, `-eo pipefail`), bash's and sh's `--rcfile` and `--init-file`, and fish's `-C`, `--init-command`, `-p`, `--profile`, `-d`, `--debug`, `-f`, `--features`, `-D`, `--debug-stack-frames`, `--profile-startup` and `--debug-output` take the next argument as their value (DEV-170, DEV-176). fish's short flags are read as its getopt reads them: in a cluster, the first of `c`, `C`, `p`, `d`, `f`, `D`, `o` takes the rest of the cluster as its value, or the next argument when it ends the cluster, and is inline code when it is `c`, so `fish -dproc script.fish` is `script.fish (fish)` and `fish -lc …` keeps its name (DEV-176). A folded chain of scripts reads `a.sh (bash) › b.sh (bash)` (DEV-159). The label is used in the table's name column, the detail pane's title and the kill modal (its title and its lists of processes); JSON, `port N` and `kill N` keep `name`, so piped `port N` stays byte-identical to v0.1.1.

Below 90 columns, where the command column is dropped, a process row's name cell fills its spare width with the arguments that follow the tool for an interpreter, or argv[0] otherwise (for a title rewritten into one argv string whose first word is the program, its text after that word: `--type=renderer …` for a Chromium child, DEV-175), joined with single spaces and cleaned, in faint text, two spaces after the label and its tags, cut with `…` at the column's edge, and left out when fewer than 6 cells remain for them. Header, container and unknown-owner rows have none. At 90 columns and wider only the label changes.

```text
NAME                                          KIND      PORTS           PID   UP
▾ api @ main (here) · 2 processes · 2 ports
    api  !  -addr :8080                       server    8080,8081       200   1d
    go  test ./...                            test                      201  30s
▾ shop @ feat/cart (worktree) · 4 processes · 1 port
  ▾ vite (node)  --port 5173                  server    *5173           101   3h
      esbuild  --service=0.21.5 --ping        other                     102   2h
    claude                                    agent                     103  20m
```

**Detail pane.** For a process, the `project` value ends with the location marker of the port answer when one applies: `api @ main · this repo`, or `shop @ feat/cart (worktree) · this repo, other worktree`. A process with a listener gets a `next free` field after `listeners`: the search from its lowest port plus one, as `port N` does for N (`next free 8082` for api on 8080 and 8081), `none in 8081-8180` when nothing in range is free, or `<error>` in the warning colour when the probe failed; no field when the lowest port is 65535. It is computed like the port line, off the UI goroutine, when the pane shows a row it has no answer for and on each new snapshot while it is open, and shows `…` until the answer arrives.

**Kill result.** When the processes a kill signalled held ports and every one of them exited, the status line first reads `killed 2 processes` as before; with the first snapshot taken after the kill finished, it adds each of those ports in ascending order: `killed 2 processes · 5173 free`, or `killed 1 process · 5173 still held by esbuild 102` (each holder by its label and pid, a container by its name, comma-joined). Free means no listener and no published container port on it in that snapshot, as `kill N` checks. After `f` force-kills the survivors, the status covers the whole kill: the processes that exited in the first round count, and their ports are listed, with the force round's (`killed 2 processes · 5173 free · 5174 free`). A process the first round did not signal (permission denied, or its pid reused) and that still runs keeps the report open after the force round too, listed with its reason (and `S rerun with sudo` where that is offered), so the status never reads as a complete kill when it was not. A forked child can hold a socket credited only to its parent, which is what this catches. A key pressed before that snapshot clears the status as any status is cleared, and the ports are not reported.

**The `other` group starts collapsed.** Decided by Doga on 2026-10-03. On a Mac `other` fills with system listeners (ControlCenter on 5000 and 7000, rapportd, launch agents) that push down everything the developer started. Its header still counts its processes and ports (`▸ other · 9 processes · 7 ports`), `→` opens it, and search finds anything inside it, so goal 1 (every listening socket is shown) holds. The fold is not remembered between runs (no config file).

**Keys.** One new key: `0`-`9` start a port search. The help overlay and README's key table list it.

## Milestones

Six phases in strict order for v1, three more for Release 1.0 and one for Release 1.1, each closed by a gate that is a test or a measurement rather than a feeling; the spike comes first because everything else rests on the platform claims it checks.

| Phase | Scope | Gate |
| --- | --- | --- |
| 0 · Spike | `devdash --json` on this machine, timings to stderr | snapshot in 100 ms or less, root-less behaviour confirmed on macOS and Linux |
| 1 · Collector and model | process table, listeners, projects, kinds, tests | tests green on both operating systems in CI with the race detector on |
| 2 · CLI | `--json` (schema v1 frozen), `port N`, `kill N` | JSON schema documented, exit codes fixed, kill verified on a spawned tree |
| 3 · Docker | socket discovery, `/containers/json`, reconciliation | a compose service's published port shows its container name on both OSes |
| 4 · TUI | table, filter, detail pane, kill modal | usable at 80x24, selection survives a refresh, golden views pass |
| 5 · Release 0.1 | goreleaser, Homebrew tap, README, demo GIF | — |
| 6 · Port answer: data | `Process.Tags`, `Project.Here`, the collector's cwd-deleted fact, JSON `tags` and `here` | tests create a server in a temporary worktree, remove the worktree and kill its parent, and see both tags on Linux CI and on a Mac; a manual run on the maintainer's Mac lists every tagged process and none is tagged wrongly |
| 7 · Port answer: commands and TUI | `port N` detail lines, `port N --json`, `devdash free N`, `next free`, the TUI's `(here)` header, tag markers and filter | `free` skips a port held by another user's listener (Linux CI as root with a second user; macOS by hand); piped `port N` output is byte-identical to v0.1.1; golden views pass |
| 8 · Release 1.0 | README, demo GIF, QA pass, tag v1.0.0 | QA finds no open bug |
| 9 · Release 1.1 | search sees hidden kinds, `other` collapsed, port search and the port line, tool labels and arguments, detail `this repo` and `next free`, the kill result's ports; QA pass, tag v1.1.0 | golden views pass, among them a port search into a collapsed group, a free port and the detail pane with `next free` at 80x24; TUI tests never bind a socket; QA finds no open bug |

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
| 6 | a runtime holds published ports in a process not in the proxy-name list (it has OrbStack's `OrbStack Helper`, Colima's `limactl`, Podman's `gvproxy` and `pasta`) | container ports show as unknown processes, and without Docker `kill N` could signal the forwarder | reconciliation also matches on port mapping alone when the owner is unknown; every proxy name is also a refused runtime name; list extended per recorded fixture |
| 7 | crowded field (portview, porthog, PortPilot, killport-tui, somo) | low adoption | project-centric scope, a README comparison, a good demo GIF |
| 8 | tree kill of a supervisor (nodemon, air) races the respawn | orphaned servers survive | parent signalled first, then descendants; survivors reported and re-killable |
| 9 | pid reuse between snapshot and kill | wrong process signalled | `(pid, start_time)` re-validated before every `kill(2)` |
| 10 | Linux `hidepid=2` or containers without userland proxy | rows missing or ownerless | warnings name the cause; documented limitation |

Open questions, to decide before Phase 2 (decisions recorded 2026-10-02, DEV-20):

- The name. `devdash` is generic; check GitHub, Homebrew, pkg.go.dev and crates.io before the first tag. **Decided (2026-10-02): keep `devdash`.** It collides with `Phantas0s/devdash` (a Go terminal dashboard, ~1,600 stars) and is taken on npm and PyPI, and is free on Homebrew core and crates.io; installs are namespaced (`dogauzun/tap/devdash`, `github.com/dogauzun/devdash`), so the collision only affects search.
- Should root-owned listeners on ports below 1024 (sshd, cups, mDNS) be hidden by default behind a `--system` flag, to keep the default view about development? **Decided: no `--system` flag.** Goal 1 shows every listening TCP socket; root-owned listeners below 1024 stay visible in the `other` group.
- Default interval: 2 s, or 1 s with the adaptive backoff carrying the load? **Decided: 2 s**, with the adaptive backoff.
- Is UDP worth including in v1 after all, given the fd walk already sees UDP sockets on both OSes? **Decided: no**, UDP stays a v1 non-goal and is not shown yet (planned for v1.1 until Release 1.1 took that number, then for v1.2 until v1.2 went to other work; since DEV-182 it carries no version number).
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
