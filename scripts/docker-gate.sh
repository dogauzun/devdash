#!/bin/sh
# Phase 3 gate (DEV-53): a compose service's published port shows its container name.
#
# Builds devdash, starts scripts/testdata/docker-gate/compose.yaml (compose project
# devdash-gate, one nginx service published on $DEVDASH_GATE_PORT, default 18080), and checks:
#   a) `devdash port N` exits 0 and names the container;
#   b) `devdash --json` lists the container, and a listener on N carries its id;
#   c) `devdash kill N --yes` (also with --tree --force) refuses with `docker stop <name>`,
#      exits 6, and the container keeps running;
#   d) the same kills with --no-docker refuse too: exit 3 when every owner of N is unknown
#      (root's docker-proxy seen by a normal user on Linux), else exit 6 (a container-runtime
#      process refused by name), and the container keeps running.
# Each kill is first run without --yes and with stdin from /dev/null: a refusal comes before
# the confirmation step and exits the same way, while a kill devdash would carry out prints
# its plan and exits 2 without signalling. Only after that dry run refused is the --yes form
# run, so a devdash whose refusal is broken fails the gate without signalling anything.
# The compose project is always taken down on exit. Needs docker with compose v2, go, jq, curl.
# CI runs it on Linux (.github/workflows/ci.yml, job docker-gate); macOS is run by hand, see
# docs/gate-phase3.md.
set -eu

port=${DEVDASH_GATE_PORT:-18080}
project=devdash-gate
name=$project-web-1

root=$(cd "$(dirname "$0")/.." && pwd)
compose_file=$root/scripts/testdata/docker-gate/compose.yaml
tmp=$(mktemp -d "${TMPDIR:-/tmp}/devdash-gate.XXXXXX")
dd=$tmp/devdash

# Container-runtime process names devdash refuses to kill by name (docs/SPEC.md, "Refused
# targets"; model.IsContainerRuntime). Used only as a guard: the script never runs a kill
# whose target is a named process outside this list, since devdash would really signal it.
runtime_names='dockerd containerd docker-proxy com.docker.backend com.docker.vpnkit vpnkit gvproxy rootlesskit rootlessport slirp4netns limactl pasta conmon'

compose() {
	DEVDASH_GATE_PORT=$port docker compose -p "$project" -f "$compose_file" "$@"
}

cleanup() {
	status=$?
	trap - EXIT
	if [ -n "${started:-}" ]; then
		compose down --volumes --remove-orphans --timeout 5 >/dev/null 2>&1 ||
			echo "warning: docker compose -p $project down failed; remove it by hand" >&2
	fi
	rm -rf "$tmp"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

last='(none)'
code=0

# run records a command's stdout, stderr and exit code without stopping the script.
run() {
	last="$*"
	code=0
	"$@" >"$tmp/out" 2>"$tmp/err" </dev/null || code=$?
}

fail() {
	{
		echo
		printf 'FAIL: %s\n' "$*"
		printf '%s\n' "--- last command: $last"
		echo "--- exit code: $code"
		echo "--- stdout:"
		cat "$tmp/out" 2>/dev/null || true
		echo "--- stderr:"
		cat "$tmp/err" 2>/dev/null || true
		echo "--- containers of $project:"
		docker ps -a --filter "label=com.docker.compose.project=$project" 2>&1 || true
		[ -x "$dd" ] || exit 1
		echo "--- devdash --json (listeners on $port and containers):"
		"$dd" --json 2>&1 </dev/null | jq --argjson port "$port" \
			'{holders: [.processes[] | select(any(.listeners[]; .port == $port))], containers, warnings}' 2>&1 || true
	} >&2
	exit 1
}

pass() {
	printf 'ok   %s\n' "$*"
}

# indent prints a file's lines under the last "ok" line, as evidence for the record.
indent() {
	sed 's/^/       /' "$1"
}

build() {
	(cd "$root" && CGO_ENABLED=0 go build -o "$dd" ./cmd/devdash)
}

need() {
	command -v "$1" >/dev/null 2>&1 || fail "$1 is not installed"
}

# still_running checks the container survived the last command and its port still answers.
still_running() {
	running=$(docker inspect -f '{{.State.Running}}' "$name" 2>&1) || fail "docker inspect $name: $running"
	[ "$running" = true ] || fail "$name is not running after: $last"
	curl -fsS -o /dev/null --max-time 5 "http://127.0.0.1:$port/" || fail "port $port no longer answers after: $last"
}

# guard fails, before any kill is run, if a JSON snapshot ($1) shows a named process with a
# listener on the port that is not marked as a container's (so kill targets the process itself,
# as cmd/devdash/kill.go's targets does) and whose name is not on the runtime list: devdash
# would signal it.
guard() {
	jq -r --argjson port "$port" '.processes[]
		| select(.pid != 0 and any(.listeners[]; .port == $port and .container == null))
		| "\(.pid):\(.name)"' "$1" >"$tmp/guard"
	while IFS= read -r owner; do
		case " $runtime_names " in
		*" ${owner#*:} "*) continue ;;
		esac
		case ${owner#*:} in
		containerd-shim*) continue ;;
		esac
		fail "port $port is held by pid ${owner%%:*} (${owner#*:}), which is neither the container's nor a known runtime process: devdash kill would signal it, so the gate stops here"
	done <"$tmp/guard"
}

# refused checks the last kill exited $1, said nothing was signalled, and (if $2 is not empty)
# named $2 on stderr.
refused() {
	[ "$code" -eq "$1" ] || fail "$last: exit $code, want $1"
	grep -qF "nothing was signalled" "$tmp/err" || fail "$last: stderr does not say nothing was signalled"
	if [ -n "$2" ]; then
		grep -qF -- "$2" "$tmp/err" || fail "$last: stderr does not contain '$2'"
	fi
}

# check_kill wants `devdash kill $port [flags...]` to refuse: exit $1, nothing signalled, stderr
# containing $2 (if not empty), and the container still running. It first runs the kill without
# --yes, stdin from /dev/null (run's), where a kill devdash would carry out stops at the
# confirmation with exit 2 instead of signalling; only if that dry run refuses as wanted does it
# run the same kill with --yes, which is what the gate checks.
check_kill() {
	want=$1
	text=$2
	shift 2
	run "$dd" kill "$port" "$@"
	if [ "$code" -ne "$want" ] && grep -qF "confirmation needs a terminal" "$tmp/err"; then
		fail "$last: devdash would kill (exit $code, the plan is on stdout) instead of refusing with exit $want; the --yes form was not run"
	fi
	refused "$want" "$text"
	still_running
	pass "devdash kill $port $* (no --yes, stdin /dev/null) refuses with exit $want"
	run "$dd" kill "$port" --yes "$@"
	refused "$want" "$text"
	still_running
	pass "devdash kill $port --yes $* exits $want and $name is still running"
	indent "$tmp/err"
}

need docker
need jq
need go
need curl
run docker compose version
[ "$code" -eq 0 ] || fail "docker compose v2 is not available"
run docker info --format '{{.ServerVersion}}'
[ "$code" -eq 0 ] || fail "the Docker daemon does not answer"

echo "== devdash Phase 3 gate: $(uname -sm), Docker $(cat "$tmp/out"), $(docker compose version --short 2>/dev/null || echo 'compose ?')"

run build
[ "$code" -eq 0 ] || fail "go build failed"
echo "== devdash $("$dd" version 2>/dev/null | head -n 1)"

run "$dd" port "$port"
case $code in
1) ;;
0) fail "port $port is not free before the test; set DEVDASH_GATE_PORT to another port" ;;
5) fail "$last: devdash failed (exit 5) before the test, see stderr" ;;
2) fail "$last: devdash rejected the command line (exit 2) before the test, see stderr" ;;
*) fail "$last: exit $code before the test, want 1 (port free)" ;;
esac

started=1
run compose up -d --quiet-pull
[ "$code" -eq 0 ] || fail "docker compose up failed"
i=0
until curl -fsS -o /dev/null --max-time 2 "http://127.0.0.1:$port/" 2>/dev/null; do
	i=$((i + 1))
	[ "$i" -lt 60 ] || fail "port $port did not answer within 60 s of compose up"
	sleep 1
done
run docker inspect -f '{{.Id}}' "$name"
[ "$code" -eq 0 ] || fail "no container named $name (compose v2 naming expected)"
cid=$(cat "$tmp/out")
pass "compose project $project is up: $name ($cid) answers on port $port"

# a) devdash port N names the container.
run "$dd" port "$port"
[ "$code" -eq 0 ] || fail "$last: exit $code, want 0"
grep -qF -- "$name" "$tmp/out" || fail "$last: output does not contain the container name $name"
pass "devdash port $port exits 0 and names $name"
indent "$tmp/out"

# b) devdash --json lists the container, and a listener on N carries its id.
run "$dd" --json
[ "$code" -eq 0 ] || fail "$last: exit $code, want 0"
cp "$tmp/out" "$tmp/snapshot.json"
jid=$(jq -r --arg name "$name" '[.containers[] | select(.name == $name)][0].id // empty' "$tmp/snapshot.json")
[ -n "$jid" ] || fail "$last: containers has no entry named $name"
[ "${#jid}" -ge 12 ] || fail "$last: container id '$jid' is shorter than 12 characters"
case $cid in
"$jid"*) ;;
*) fail "$last: container id $jid is not Docker's id $cid" ;;
esac
jq -e --arg id "$jid" --arg project "$project" --argjson port "$port" '
	.containers[] | select(.id == $id)
	| .state == "running" and .compose_project == $project and .compose_service == "web"
	  and any(.ports[]; .host_port == $port and .container_port == 80 and .proto == "tcp")' \
	"$tmp/snapshot.json" >/dev/null ||
	fail "$last: container $name lacks state running, compose labels $project/web or port $port->80/tcp"
pass "devdash --json lists $name ($jid): running, compose $project/web, $port->80/tcp"
# The listener's container, not the process's: Docker Desktop's one com.docker.backend holds
# every container's ports, so its process-level container is null once another container
# publishes a port (docs/json-schema.md).
jq -e --arg id "$jid" --argjson port "$port" \
	'any(.processes[].listeners[]; .port == $port and .container == $id)' \
	"$tmp/snapshot.json" >/dev/null || fail "$last: no listener on port $port has \"container\": \"$jid\""
pass "devdash --json: a listener on port $port has \"container\" set to $name's id"
jq -r --argjson port "$port" '.processes[] | select(any(.listeners[]; .port == $port))
	| "pid \(.pid) \(.name) kind \(.kind) listener containers \([.listeners[] | select(.port == $port) | .container])"' \
	"$tmp/snapshot.json" | sed 's/^/       /'

# c) kill refuses a container's port and names docker stop.
guard "$tmp/snapshot.json"
check_kill 6 "docker stop $name"
check_kill 6 "docker stop $name" --tree --force

# d) without Docker devdash cannot name the container, and still refuses.
run "$dd" --json --no-docker
[ "$code" -eq 0 ] || fail "$last: exit $code, want 0"
cp "$tmp/out" "$tmp/nodocker.json"
jq -e '.containers == []' "$tmp/nodocker.json" >/dev/null || fail "$last: containers is not []"
owners=$(jq --argjson port "$port" '[.processes[] | select(any(.listeners[]; .port == $port))]' "$tmp/nodocker.json")
[ "$(printf '%s\n' "$owners" | jq 'length')" -gt 0 ] ||
	fail "$last: nothing listens on port $port without Docker (userland proxy disabled?), so there is nothing for kill to refuse"
[ "$(printf '%s\n' "$owners" | jq '[.[] | .container, .listeners[].container] | all(. == null)')" = true ] ||
	fail "$last: a process or listener has a container id with --no-docker"
guard "$tmp/nodocker.json"
# Exit 3 when every owner is unknown (PID 0: more permission would show it), else 6 (an owner
# refused by name as part of the container runtime) - cmd/devdash/kill.go, DECISIONS (DEV-25).
if [ "$(printf '%s\n' "$owners" | jq 'all(.pid == 0)')" = true ]; then
	want=3
	why="owner unknown"
else
	want=6
	why="container runtime refused by name"
fi
pass "devdash --json --no-docker: port $port held by $(printf '%s\n' "$owners" | jq -r '[.[] | "\(.pid) \(.name)"] | join(", ")') ($why), want exit $want"
check_kill "$want" "" --no-docker
check_kill "$want" "" --no-docker --tree --force

echo "PASS: Phase 3 gate on $(uname -s)"
