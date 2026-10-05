# Known limitations

What devdash cannot see or do, and the workaround where there is one.

- **Other users' processes.** Without sudo, their rows are shown but argv, cwd, CPU and
  memory may be unknown, and their listeners have no owner (pid 0, "owner unknown"). devdash
  shows a warning instead of hiding them. On Linux, root in a container with default
  capabilities (no `CAP_SYS_PTRACE`) is in the same position; `--cap-add SYS_PTRACE` fixes it
  (see [DECISIONS.md](https://github.com/dogauzun/devdash/blob/main/DECISIONS.md), DEV-13).
  When sudo would show more (and after a kill denied for permission), the dashboard offers `S`: after you confirm with `y` it quits and
  starts again as `sudo devdash` with the same flags; sudo asks for your password itself.
  It is never offered as root, in such a container, or for Docker's socket permission.
- **Linux `hidepid`.** With `/proc` mounted `hidepid=1` or `2` (`noaccess` or `invisible`),
  other users' processes are invisible and their listeners stay without an owner. devdash
  warns and names the mount option.
- **macOS socket list.** Other users' listeners come from the kernel's TCP socket list, which
  macOS withholds when an ancestor of devdash is ad-hoc signed (for example `go run`). An
  empty list is treated as unknown, not as "no listeners", and a footer hint says so. Start
  devdash directly from a shell, or use sudo. Your own listeners are always found. See the
  Signing notes in
  [docs/SPEC.md](https://github.com/dogauzun/devdash/blob/main/docs/SPEC.md#build-release-and-distribution)
  and [DECISIONS.md](https://github.com/dogauzun/devdash/blob/main/DECISIONS.md) (DEV-10).
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
- **UDP and unix sockets** are not shown yet.
- **Windows** is not supported. Neither are remote hosts, a config file or a background
  daemon: devdash runs only while its terminal is open.
