# Using devdash

The full reference: the dashboard, every command, its flags and its exit codes.
[README](../README.md) has the short version; `devdash --help` prints a summary.

## The dashboard

```sh
devdash
```

One screen: a header, a table grouped by project, and a footer with key hints and warnings.
It is meant to be usable at 80 columns by 24 rows. `?` lists the keys (see
[Keybindings](../README.md#keybindings)) and `q` quits.

**Groups.** Each project header shows `name @ branch (worktree)`. Processes with no project go
under `other`, which starts collapsed (its header still counts them; `→` opens it), and
containers without a compose project under `containers`.

**Rows.** Inside a group, processes form a tree by parent pid, and each has a kind: agent,
test, watcher, editor, shell, server, container or other. Shells and editors are hidden unless
`--all` is given or toggled in the dashboard. A process run by an interpreter is named by its
tool, as in `vite (node)` or `pytest (python3)`, and below 90 columns, where the command column
does not fit, its arguments follow the name in faint text. A detail pane shows the full argv,
cwd, listeners, parent chain and start time.

**Chains.** A chain of processes that each have one child is one row, the last process's, named
by the chain less the shells and editors the view hides (`claude › xargs`, or
`bash › claude › bash › xargs` with `--all`): `→` unfolds it and `←` on its first row folds it
again. An unfolded chain has a row for each process its label names, none for a shell or editor
the view hides. A process with a port or a tag always keeps its own row. On a folded row, `x` kills the
last process, and `t` in the kill modal switches to the whole chain as the row names it, so
`nodemon (node) › server.js (node)` stops nodemon and its server together.

**Filter.** The `/` filter searches every row, folded or hidden: a shell or editor that matches
shows without `a`, and a container row with `d`.

**Port search.** Typing a port number in the table, `5173` say, searches for it and selects the
process or container holding it, even inside a collapsed group, so `enter` (which closes the
filter prompt) and then `x` kills it, or `enter` again opens the detail pane; a line under the
header says how many rows hold the port and which port is free next (the search
`devdash free` runs), or that the port is free.

| Exit code | Meaning |
| --- | --- |
| 0 | quit with `q`, `ctrl-c`, SIGINT, SIGTERM or SIGHUP |
| 2 | usage error, or stdout is not a terminal (piped, redirected, cron): use `--json`, `port N` or `free N` there |
| 5 | devdash failed |

## `devdash --json`

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

## `devdash port N`

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
[docs/json-schema.md](json-schema.md#port_answer)), with the same exit codes.

| Exit code | Meaning |
| --- | --- |
| 0 | something listens on N |
| 1 | the port is free |
| 2 | usage error |
| 5 | devdash failed, so it could not look |

Exit 1 only ever means "free", so `devdash port 3000 || npm run dev` never starts a second
server because devdash failed.

## `devdash free N`

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

## `devdash kill N`

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

## `devdash version`

Prints the version, the commit, and the commit's date (labelled `built`: the commit time, not
the build time). Exits 0. What a binary knows depends on how it was built:

- Release archives and the Homebrew cask (which installs the archive's binary) record all three:
  `devdash v1.1.0 (commit dbadac7e5ba856d776e36251559b8abd03de2654, built 2026-10-04T10:31:26Z)`.
- `go install github.com/dogauzun/devdash/cmd/devdash@latest` (or `@v1.1.0`) records the
  version only, since a module from the proxy carries no git data:
  `devdash v1.1.0 (commit none, built unknown)`.
- `make build`, `go build` or `go install` inside a git checkout record the commit and its time,
  with a Go pseudo-version as the version; uncommitted changes add `+dirty` to the version and
  `-dirty` to the commit:
  `devdash v1.1.1-0.20261005093007-6d4b5d0a89f2 (commit 6d4b5d0a89f28ee01e784aca358dd6cfd67125b7, built 2026-10-05T09:30:07Z)`.

## Global flags

Flags may come before or after the command. `-h` or `--help` prints the usage and exits 0.

| Flag | Effect |
| --- | --- |
| `--roots paths` | only count git repositories under these directories; comma-separated and repeatable; a leading `~` is `$HOME` |
| `--tick d` | dashboard refresh interval (default 2s, minimum 500ms) |
| `--all` | show shells and editors in the dashboard (`--json` always lists every process) |
| `--no-docker` | do not ask Docker for containers |
| `--no-color` | no colour; also when `NO_COLOR` is set and not empty |
| `--json` | print one snapshot as JSON |
