# `devdash --json` schema, version 1

`devdash --json` prints one snapshot as one JSON document on stdout and exits 0; stderr stays empty on success. If no snapshot can be taken, or stdout cannot be written, it exits 5 with the error on stderr and nothing on stdout (a failed write may leave part of the document). `cmd/devdash/json_test.go` checks real output against the tables below, so every field is listed here.

## Conventions

| Rule | Value |
| --- | --- |
| Versioning | `schema_version` is 1. Removing or renaming a field, or changing its type or unit, bumps it; adding a field or a `timing_ms` key, `unknown` name, warning code or `kind` value does not. |
| Keys | Every key in the tables is always present, except `timing_ms` keys marked otherwise. Key order is stable but not part of the contract. |
| Absent values | `null`, never `""`, `0` or `[]` as a stand-in. A list that is known and empty is `[]`; a list that could not be read is `null`. |
| Unreadable fields | Named in the process's `unknown` list, and their value is `null`. |
| Strings | UTF-8. A byte that is not valid UTF-8 (in `argv`, `cwd`, `name`, a project path) is written as U+FFFD, so such a value cannot be turned back into the original bytes. |
| Times | RFC 3339 in UTC, `Z` suffix, with the fraction the OS gives (Linux: 10 ms; macOS: 1 µs), trailing zeros dropped. |
| Sizes | Bytes. |
| Durations | Milliseconds, a number with up to 3 decimals (1 µs). |
| CPU | Two samples are taken 200 ms apart; `cpu_percent` is the CPU time used between them over the wall time between them, top-style: one fully used core is 100, and a multi-threaded process can exceed 100. Rounded to 2 decimals. |
| Processes | Every process, including shells and editors: `--all` only affects the dashboard. |
| Order | `processes` in collector order (by pid on Linux), then one `pid: 0` entry per listener with no readable owner. `projects` in order of first use by `processes`. |

## snapshot

The top-level object.

| Field | Type | Present | Description |
| --- | --- | --- | --- |
| `schema_version` | integer | always | 1 |
| `taken_at` | string | always | When the second sample started (RFC 3339, UTC). |
| `host` | host | always | The machine and the user devdash ran as. |
| `projects` | project array | always | Git repositories with at least one process. |
| `processes` | process array | always | Every process, then the unknown-owner entries. |
| `containers` | container array | always | Running Docker or Podman containers; `[]` with `--no-docker`, when no engine is found or while it does not answer. |
| `warnings` | warning array | always | Degraded-mode conditions, one per code. |
| `timing_ms` | timing_ms | always | Per-source durations of the second sample. |

## host

| Field | Type | Present | Description |
| --- | --- | --- | --- |
| `os` | string | always | `darwin` or `linux` (Go's `GOOS`). |
| `arch` | string | always | `arm64`, `amd64` (Go's `GOARCH`). |
| `hostname` | string or null | always | As the OS reports it; `null` when it cannot be read. |
| `uid` | integer | always | Effective uid devdash ran as. |

## project

| Field | Type | Present | Description |
| --- | --- | --- | --- |
| `id` | string | always | Repository root path; the value of a process's `project`. |
| `root` | string | always | Repository root path (same as `id`). |
| `name` | string | always | Basename of `root`, or of `main_repo` for a linked worktree. |
| `branch` | string or null | always | Checked-out branch; `null` when HEAD is detached or unreadable. |
| `short_sha` | string or null | always | First 7 characters of a detached HEAD; `null` on a branch. |
| `worktree` | boolean | always | `true` for a linked worktree. |
| `main_repo` | string or null | always | Main repository's work tree for a linked worktree, else `null`. |

## process

| Field | Type | Present | Description |
| --- | --- | --- | --- |
| `pid` | integer | always | Process id; `0` for a listener whose owner could not be read (the "unknown owner" entry). |
| `ppid` | integer or null | always | Parent pid; `null` when there is no parent (the kernel reports 0: pid 1, launchd, a container's init) and for `pid: 0`. |
| `start_time` | string or null | always | Start time (RFC 3339, UTC). With `pid` it identifies the process, since pids are reused. `null` for `pid: 0`. |
| `uid` | integer or null | always | Effective uid; `null` for `pid: 0`. |
| `user` | string or null | always | User name of `uid`, or the uid as a string when it has no name; `null` for `pid: 0`. |
| `name` | string | always | Kernel name (Linux comm, 15 bytes; macOS p_comm, 16), or the basename of `argv[0]` when the kernel name was cut short. `unknown` for `pid: 0`. |
| `argv` | string array or null | always | Command line; `[]` when the process has none (blanked, or mid-exec), `null` when unreadable (`argv` in `unknown`). |
| `cwd` | string or null | always | Working directory; `null` when unreadable (`cwd` in `unknown`). |
| `cpu_percent` | number or null | always | See Conventions, CPU. `null` when unreadable or when the process was not in the first sample (`cpu` in `unknown`). |
| `rss_bytes` | integer or null | always | Resident memory in bytes; `null` when unreadable (`mem` in `unknown`). |
| `listeners` | listener array | always | Listening TCP sockets this process owns; `[]` for none. |
| `kind` | string | always | One of `other`, `server`, `container`, `agent`, `test`, `watcher`, `shell`, `editor`. `other` for `pid: 0`, or `container` when its socket matched a container's published port. |
| `project` | string or null | always | `id` of the project the process belongs to, `null` for none. |
| `container` | string or null | always | Id of the container whose published ports this process holds, when every one of its sockets is that one container's; `null` otherwise, including for a proxy holding several containers' ports or one container's next to a port of its own (`OrbStack Helper`, `com.docker.backend`; see the listener's `container`). |
| `unknown` | string array | always | Fields that could not be read, in this order: `owner`, `argv`, `cwd`, `cpu`, `mem`. `[]` when everything was read. `pid: 0` entries have all five. |

## listener

| Field | Type | Present | Description |
| --- | --- | --- | --- |
| `proto` | string | always | `tcp4` or `tcp6`. A dual-stack socket is one `tcp6` listener on `::`; a v4-mapped bind is `tcp4`. |
| `addr` | string | always | Bind address; `0.0.0.0` or `::` means every interface. A scoped IPv6 address (link-local) carries its zone on macOS: `fe80::1%lo0`. |
| `port` | integer | always | TCP port, 1 to 65535. |
| `container` | string or null | always | Id of the container whose published port this socket is (matched against Docker's port list); `null` otherwise. Set on each socket of a proxy that holds several containers' ports (Docker Desktop's `com.docker.backend`), whose process `container` is `null`. |

## container

| Field | Type | Present | Description |
| --- | --- | --- | --- |
| `id` | string | always | Container id. |
| `name` | string | always | Container name, without the leading `/`. |
| `image` | string | always | Image reference. |
| `state` | string | always | `running`, `paused`, `restarting`, ... as Docker reports it. |
| `compose_project` | string or null | always | Label `com.docker.compose.project`; `null` when absent. |
| `compose_service` | string or null | always | Label `com.docker.compose.service`; `null` when absent. |
| `ports` | port_mapping array | always | Port mappings; `[]` for none. |

## port_mapping

| Field | Type | Present | Description |
| --- | --- | --- | --- |
| `host_ip` | string or null | always | Host address the port is published on; `null` when Docker reports none. |
| `host_port` | integer or null | always | Published host port; `null` when the port is only exposed, not published. |
| `container_port` | integer | always | Port inside the container. |
| `proto` | string | always | `tcp` or `udp`. |

## warning

| Field | Type | Present | Description |
| --- | --- | --- | --- |
| `code` | string | always | Stable snake_case code, see Warning codes. |
| `count` | integer | always | How many processes, listeners or occurrences it covers. |
| `hint` | string | always | One line for a human; wording may change without a version bump. |

## timing_ms

| Field | Type | Present | Description |
| --- | --- | --- | --- |
| `proctable` | number | always | Process list and basic facets. |
| `argv_cwd` | number | always | argv, cwd, CPU time and memory of each process. |
| `listeners` | number | always | Listening sockets and their owners. |
| `pcblist` | number | on macOS | Part of `listeners`: the `net.inet.tcp.pcblist_n` read. |
| `projects` | number | always | Project resolution. |
| `docker` | number | when Docker is configured | The Docker fetch the snapshot used, run alongside collection. Absent with `--no-docker` or when no endpoint was found. |
| `total` | number | always | Wall time of the second sample: collection, waiting for the Docker fetch, building the snapshot and naming users. |

## Warning codes

| Code | OS | Meaning |
| --- | --- | --- |
| `listener_owner_unreadable` | both | Listeners whose owner could not be read; `count` is the number of `pid: 0` entries whose listener has `container` `null`. An entry matched to a container (root's `docker-proxy` seen by a normal user) is explained and not counted, and the warning is absent when no entry is left. |
| `process_fields_unreadable` | both | Processes of other users with fields in `unknown`; `count` is the number of processes. |
| `pcblist_unavailable` | macOS | The kernel withheld other processes' sockets from the PCB list; other users' listeners may be missing. |
| `proc_hidepid` | Linux | `/proc` is mounted with `hidepid`; other users' processes are invisible. |
| `docker_endpoint_invalid` | both | `DOCKER_HOST` or the docker context names an endpoint devdash cannot use. |
| `docker_unreachable` | both | A Docker socket exists but does not answer. |
