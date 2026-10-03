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

# The leftover vite: its parent exited and its worktree is gone. The detail pane says so.
Type "/"
Sleep 400ms
Type@120ms "orphaned"
Sleep 1s
Enter
Down
Sleep 800ms
Enter
Sleep 4.5s
Escape
Sleep 500ms
Escape
Sleep 1s

# Kill nodemon and its child together: x shows the plan, t switches it to the tree.
Type "/"
Sleep 400ms
Type@120ms "nodemon"
Sleep 800ms
Enter
Down
Sleep 800ms
Type "x"
Sleep 2s
Type "t"
Sleep 3s
Enter
Sleep 2.5s
Escape
Sleep 2.5s
Type "q"
Sleep 800ms

# Who holds 5174, should it go, and which port is free instead.
Type "devdash port 5174"
Sleep 400ms
Enter
Sleep 4.5s
Type "devdash port 5173"
Sleep 400ms
Enter
Sleep 4s
Type "devdash free 5173"
Sleep 400ms
Enter
Sleep 3.5s
