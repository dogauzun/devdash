#!/bin/sh
# The dev machine that demo.tape records for docs/demo.gif (DEV-65). Re-record with `make demo`.
#
# Builds devdash and scripts/demoproc, then enters new PID, mount, UTS and network namespaces,
# so the dashboard sees only the demo's processes and sockets and none of the host's. Inside,
# it sets the hostname to dev-box, mounts a tmpfs on /home, /run and /var/run (no host Docker
# or Podman socket), and creates four repositories under /home/me/code with something running
# in each:
#   shop        (main)                 nodemon server.js, whose child node server.js serves :3000
#   shop-cart   (feat/cart, a linked   vite --port 5173, vitest --watch
#                worktree of shop)
#   blog        (main)                 python3 -m http.server --bind 0.0.0.0 4000
#   api         (fix/timeouts)         air, whose child api serves :8080; claude
# and a leftover: vite --port 5174, started in shop-search (feat/search, another linked worktree
# of shop), whose parent exited and whose worktree was then removed, so it carries the orphaned
# and cwd deleted tags. The dev tools are demoproc linked under their names; nodemon, vite and
# vitest run as node node_modules/.bin/<tool>, as npm scripts start them, so their rows read
# vite (node) and vitest (node), and the two chains fold into one row each: nodemon (node) ›
# server.js (node) and air › api. When every port listens, it clears the screen
# and runs an interactive bash with the prompt "$ " in /home/me. A subshell that plays
# the terminal is the parent of that bash and of every other dev tool, so only the leftover has
# pid 1 as its parent; exiting the bash ends the namespace and every process in it.
#
# Linux only, as root (unshare). Needs go, git and python3.
set -eu

if [ "${DEMO_INSIDE:-}" != 1 ]; then
	repo=$(cd "$(dirname "$0")/.." && pwd)
	bin=$(mktemp -d "${TMPDIR:-/tmp}/devdash-demo.XXXXXX")
	(cd "$repo" && CGO_ENABLED=0 go build -trimpath -o "$bin/devdash" ./cmd/devdash)
	(cd "$repo" && CGO_ENABLED=0 go build -trimpath -o "$bin/demoproc" ./scripts/demoproc)
	DEMO_INSIDE=1 DEMO_BIN=$bin exec unshare --pid --fork --mount-proc --uts --net sh "$0"
fi

# Only the unshare --pid --fork child is pid 1: never touch the host's hostname or mounts.
[ "$$" = 1 ] || {
	echo "demo.sh: not in the demo namespaces; run it without DEMO_INSIDE" >&2
	exit 1
}
# pid 1 stays for the whole demo: keep it out of the repository this script came from.
cd /
hostname dev-box
mount -t tmpfs tmpfs /home
# No container engine of the host: hide its sockets and drop the variables that name one.
mount -t tmpfs tmpfs /run
[ -L /var/run ] || mount -t tmpfs tmpfs /var/run
unset DOCKER_HOST DOCKER_CONTEXT DOCKER_CONFIG XDG_RUNTIME_DIR
export HOME=/home/me
mkdir -p "$HOME/bin" "$HOME/code"
cp "$DEMO_BIN/devdash" "$DEMO_BIN/demoproc" "$HOME/bin/"
rm -rf "$DEMO_BIN"
for name in nodemon node vite vitest air api claude; do
	ln "$HOME/bin/demoproc" "$HOME/bin/$name"
done
export PATH="$HOME/bin:$PATH"
export GIT_AUTHOR_NAME=me GIT_AUTHOR_EMAIL=me@example.com
export GIT_COMMITTER_NAME=me GIT_COMMITTER_EMAIL=me@example.com

code=$HOME/code
repo() { # repo DIR BRANCH: a repository with one commit on BRANCH
	git init -q -b "$2" "$1"
	echo "# $(basename "$1")" >"$1/README.md"
	git -C "$1" add README.md
	git -C "$1" commit -q -m "first commit"
}
repo "$code/shop" main
git -C "$code/shop" worktree add -q -b feat/cart "$code/shop-cart"
repo "$code/blog" main
repo "$code/api" fix/timeouts

start() { # start DIR COMMAND...: COMMAND in the background, in DIR, with no terminal
	dir=$1
	shift
	(cd "$dir" && exec "$@" </dev/null >/dev/null 2>&1) &
}
# The leftover: a dev server started in a worktree, left running when its terminal closed (its
# parent is pid 1), and still running after the worktree was removed.
git -C "$code/shop" worktree add -q -b feat/search "$code/shop-search"
start "$code/shop-search" node node_modules/.bin/vite --port 5174
until devdash port 5174 >/dev/null 2>&1; do sleep 0.1; done
git -C "$code/shop" worktree remove --force "$code/shop-search"

# Everything else runs under a subshell that plays the terminal: it starts the dev tools and
# then the interactive bash, so neither they nor that bash (which the demo cds into a
# repository) have pid 1 as their parent. pid 1 only waits.
(
	start "$code/shop" env PORT=3000 DEMO_CHILD="node server.js" node node_modules/.bin/nodemon server.js
	start "$code/shop-cart" node node_modules/.bin/vite --port 5173
	start "$code/shop-cart" node node_modules/.bin/vitest --watch
	start "$code/blog" python3 -m http.server --bind 0.0.0.0 4000
	start "$code/api" env PORT=8080 DEMO_CHILD=api air
	start "$code/api" claude

	for port in 3000 5173 4000 8080; do
		until devdash port "$port" >/dev/null 2>&1; do sleep 0.1; done
	done

	cd "$HOME"
	clear
	PS1='$ ' bash --norc --noprofile -i
	: # dash would exec a last command, making bash a child of pid 1
)
