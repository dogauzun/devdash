# The README demo (DEV-65): `make demo` records docs/demo.gif from this file with vhs
# (https://github.com/charmbracelet/vhs). scripts/demo.sh builds devdash and starts the
# scripted dev machine it shows: four repositories with dev servers, watchers, a test runner
# and an agent, plus a leftover dev server from a removed worktree, alone in their own
# namespaces. Linux only, as root.

Output docs/demo.gif

Require go
Require git
Require python3
Require unshare

Set Shell bash
Set FontSize 16
Set Width 1400
Set Height 500
Set Padding 16
Set TypingSpeed 70ms
Set Theme "Catppuccin Mocha"

Hide
Type "sh scripts/demo.sh"
Enter
Wait@3m /^\$\s*$/
Show

Sleep 500ms
Type "cd code/shop"
Enter
Sleep 300ms
Type "devdash"
Sleep 400ms
Enter
Sleep 4.5s

# Who holds 5173? Digits start a port search: the holder is selected in its group, and the
# detail pane places it in this repo, in another worktree, with the next free port.
Type@200ms "5173"
Sleep 1.5s
Enter
Sleep 500ms
Enter
Sleep 4.5s
Escape
Sleep 500ms
Escape
Sleep 800ms

# Who holds 5174? Digits start a port search: the holder is selected wherever it is, and the
# port line under the header names the next free port. The detail pane says why it is still
# running (its parent exited, its worktree is gone). It is outside every project, so x asks
# twice; then the status line and the port line say 5174 is free.
Type@200ms "5174"
Sleep 2.5s
Enter
Sleep 500ms
Enter
Sleep 4.5s
Escape
Sleep 500ms
Type "x"
Sleep 1.5s
Enter
Sleep 2s
Type "Y"
Sleep 4.5s
Escape
Sleep 1s

# Kill nodemon and its child together. They fold into one row, nodemon (node) › server.js
# (node), which is server.js: → unfolds the chain with server.js selected, ↑ selects nodemon,
# x shows the plan, t switches it to the tree.
Down
Sleep 1.2s
Right
Sleep 1.5s
Up
Sleep 800ms
Type "x"
Sleep 2s
Type "t"
Sleep 3s
Enter
Sleep 4s
Type "q"
Sleep 800ms

# The same answers outside the dashboard.
Type "devdash port 5173"
Sleep 400ms
Enter
Sleep 4.5s
Type "devdash free 5173"
Sleep 400ms
Enter
Sleep 3.5s
