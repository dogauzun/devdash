---
name: run-devdash
description: Run the devdash dashboard and drive it from an agent session - send keys, read the screen as text, and look at a known process tree. Use to see a TUI change working in the real app, not only in tests.
---

# Run devdash

The dashboard takes over the terminal, so an agent drives it through `drive.py` in this
directory: it runs the binary in a pty, types each step's keys and prints the screen as text.
It needs only Python 3. tmux is not required (it is not installed on the maintainer's Mac, and
the built-in `screen` 4.00.03 captures blank frames from Bubble Tea v2).

## Run (for agents)

From the repository root:

```bash
make build
python3 .claude/skills/run-devdash/drive.py --lines 12 '<down><down>' '<enter>' '?'
```

The screen is printed once at start and once after every step:

```
=== start
  mbp · 1 s ago · 6 projects · 11 listeners · 0 containers
  NAME                                KIND      PORTS           PID   UP   CPU   MEM USER     COMMAND
» ▾ chain @ main (here) · 8 processes · 0 ports
    ▾ bash  orphaned                  shell                   53942   4s   0.0    2M dogauzun /bin/bash ./link.sh 4
```

- `» ` marks the selected row. It is found by reverse video, which is why the default command
  is `./devdash --no-color`.
- A step is literal keys plus names in angle brackets: `<up> <down> <left> <right> <enter>
  <esc> <tab> <backspace> <pgup> <pgdown> <home> <end> <ctrl-c> <ctrl-u>`, and `<wait:3>` to
  let a refresh tick pass (`--tick`, 2 s by default). `'/vite'` types a filter, `'5173'` a
  port search.
- `--size 140x40` is the default. The golden views are 80x24 and 120x40; below 100 columns
  cpu, mem and user are dropped, below 90 the command.
- `--cwd DIR` is where the dashboard runs; the repository there is its `(here)` project and is
  listed first.
- `--cmd "./devdash --no-color --no-docker"` changes the command or its flags; `--lines N`
  prints only the top of each screen.
- The driver quits the dashboard itself (`esc`, `q`, then SIGTERM).

## A known process tree

The dashboard shows this machine's real processes. For a tree you control, `chain.sh` starts
nested scripts ending in two sleeps, in a throwaway git repository:

```bash
dir=$(mktemp -d)/chain
.claude/skills/run-devdash/chain.sh start "$dir" 4      # prints the pids it started
python3 .claude/skills/run-devdash/drive.py --cwd "$dir" --lines 9 '<down><down>' '<right>' 'x'
.claude/skills/run-devdash/chain.sh stop "$dir"         # stops exactly those pids
```

The first script is re-parented to launchd or init, so its row carries the `orphaned` tag
and keeps a row of its own; the scripts below it fold into one row.

## Keys

| Key | Action |
|---|---|
| `↑` `↓` `j` `k` | move the selection |
| `←` `→` `h` `l` | collapse or expand a group or node; `→` unfolds a chain, `←` on its first row refolds |
| `enter` | open or close the detail pane (`esc` closes it too) |
| `/` | filter; `esc` clears |
| `0`-`9` | port search |
| `x` | kill modal: `p` process, `t` tree, `f` force, `enter` confirm, `esc` cancel |
| `o` | open `http://localhost:<port>` in the browser |
| `a` `d` `s` `r` | shells and editors, container rows, sort, refresh |
| `S` | rerun under sudo, after `y` |
| `?` | help |
| `q` `ctrl-c` | quit |

`internal/tui/help.go` is the authority.

## Safe use on a real machine

- `x` opens the kill plan; read it and send `<esc>`. Send `<enter>` (and `Y` for a process
  outside every project) only for a process this session started, such as the chain above.
- Never send `y` after `S`: it replaces the dashboard with `sudo`, which waits for a password.
- `o` opens the user's browser; avoid it unless the task is about `o`.
- The `other` group starts collapsed and holds every process outside a project.

## Run (for humans)

```bash
make build && ./devdash
```

`?` lists the keys, `q` quits.
