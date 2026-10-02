# The README demo (DEV-65): `make demo` records docs/demo.gif from this file with vhs
# (https://github.com/charmbracelet/vhs). scripts/demo.sh builds devdash and starts the
# scripted dev machine it shows: four repositories with dev servers, watchers, a test runner
# and an agent, alone in their own namespaces. Linux only, as root.

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
Type "devdash --roots ~/code"
Sleep 400ms
Enter
Sleep 3.5s

# Find the process on port 5173 and open its detail pane.
Type "/"
Sleep 400ms
Type@200ms "5173"
Sleep 1s
Enter
Down
Sleep 800ms
Enter
Sleep 4s
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
Sleep 3s

Type "?"
Sleep 3.5s
Type "q"
Sleep 400ms
Type "q"
Sleep 800ms

# Port 3000 is free again.
Type "devdash port 3000"
Sleep 400ms
Enter
Sleep 3s
