#!/bin/bash
# A known process tree to look at in the dashboard: a chain of nested scripts in a throwaway
# git repository, ending in two sleeps.
#
#   chain.sh start DIR [DEPTH]   create DIR as a git repository and start the chain (default depth 4)
#   chain.sh stop DIR            stop exactly the processes that start recorded (pid, start time, command)
set -eu

dir=${2:?usage: chain.sh start|stop DIR [DEPTH]}

tree() { # pid and every descendant, parents first
	echo "$1"
	for c in $(pgrep -P "$1"); do tree "$c"; done
}

ident() { # what identifies a process besides its pid: start time and command
	ps -ww -o lstart= -o command= -p "$1" 2>/dev/null # -ww: never cut to the terminal width
}

case $1 in
start)
	mkdir -p "$dir"
	dir=$(cd "$dir" && pwd -P)
	[ ! -e "$dir/.chain" ] || { echo "a chain is recorded in $dir: stop it first" >&2; exit 1; }
	git -C "$dir" init -q
	cat >"$dir/link.sh" <<'EOF'
#!/bin/bash
# Link N runs link N-1 and waits for it (the `true` keeps bash from exec'ing its last command).
if [ "$1" -gt 0 ]; then
	"$0" $(($1 - 1))
	true
else
	sleep 600 &
	sleep 600 &
	wait
fi
EOF
	chmod +x "$dir/link.sh"
	cd "$dir"
	nohup ./link.sh "${3:-4}" >/dev/null 2>&1 &
	root=$!
	sleep 1
	for p in $(tree "$root"); do echo "$p $(ident "$p")"; done >"$dir/.chain"
	cat "$dir/.chain"
	;;
stop)
	while read -r pid was; do # only the recorded process: a reused pid has another start time
		if [ "$(ident "$pid")" = "$was" ]; then kill "$pid" && echo "stopped $pid $was"; fi
	done <"$dir/.chain"
	rm -f "$dir/.chain"
	;;
*)
	echo "usage: chain.sh start|stop DIR [DEPTH]" >&2
	exit 2
	;;
esac
