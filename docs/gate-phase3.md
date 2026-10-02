# Phase 3 gate

The Phase 3 gate (docs/SPEC.md, Milestones) is "a compose service's published port shows its container name on both OSes". `scripts/docker-gate.sh` checks it. The script builds devdash and starts a one-service compose project, `devdash-gate`, whose `nginx:1.27-alpine` container `devdash-gate-web-1` is published on port 18080. It then checks four things:

- `devdash port 18080` names the container.
- `devdash --json` lists the container, and a listener on 18080 has its id in the listener's `container`. The process-level `container` is not used: Docker Desktop's one `com.docker.backend` holds every container's ports, so it is `null` once another container publishes a port.
- `devdash kill 18080 --yes`, plain and with `--tree --force`, refuses with `docker stop devdash-gate-web-1`, exits 6, and leaves the container running.
- The same kills with `--no-docker` refuse too and leave the container running. They exit 3 when every owner of the port is unknown, as with root's `docker-proxy` seen by a normal user on Linux. They exit 6 when the owner is a container-runtime process refused by name.

Each kill is first run without `--yes`, with stdin from `/dev/null`. devdash refuses before it asks for confirmation, so a refusal gives the same exit code and "nothing was signalled". A kill devdash would carry out prints its plan and exits 2 ("confirmation needs a terminal") without signalling. The `--yes` form runs only after that dry run refused as expected. A devdash whose refusal is broken therefore fails the gate without signalling the port's owner, which on a Mac would be Docker Desktop.

The compose project is always taken down when the script exits. On Linux, CI runs the script in the `docker-gate` job.

## Phase 3 gate (macOS, by hand)

GitHub's macOS runners have no Docker, so the macOS half is run on a Mac and pasted into the PR that closes Phase 3.

1. Start Docker Desktop and wait until `docker info` answers. The gate is defined against Docker Desktop, where `com.docker.backend` holds published ports. If another runtime (OrbStack, Colima) holds the port with a process that is not on the spec's runtime list, the script stops before any kill instead of signalling that process.
2. Have Go (the version in `go.mod`), `jq` (in `/usr/bin` since macOS 15, else `brew install jq`) and `curl`.
3. From the repository root, in a Terminal.app or iTerm shell, run:

   ```sh
   sh scripts/docker-gate.sh 2>&1 | tee gate-macos.txt
   ```

   Do not run it inside a Homebrew-built tmux or another ad-hoc-signed parent: macOS then withholds other processes' sockets from devdash (DECISIONS, DEV-10), and the listener on 18080 would be missing. If port 18080 is taken, set another one with `DEVDASH_GATE_PORT=18081 sh scripts/docker-gate.sh`.
4. The run passes when the last line is `PASS: Phase 3 gate on Darwin`. Each check prints an `ok` line followed by its evidence. On a failure the script prints `FAIL:` with the command, its exit code, stdout, stderr, the compose containers and the port's holders from `devdash --json`.

In the PR, add a section titled "Phase 3 gate, macOS". Put in it the output of `sw_vers -productVersion`, `uname -m` and Docker Desktop's version (Docker Desktop, About), followed by the full contents of `gate-macos.txt` in a fenced code block. Paste a failing run as well: per the spec, a failed gate sends the work back into Phase 3.
