package model

import (
	"path"
	"strings"
)

// Classify returns the kind of p from the basename of argv[0], the rest of argv and its
// listeners, by the first matching rule of the spec's table ("Process tree and kinds"):
// agent, test, watcher, editor, shell, then server when p has a listener, else other.
// It never returns KindContainer: that kind is set by Reconcile. The PID 0 "unknown owner"
// pseudo-process is always KindOther (spec JSON example); another process with no argv
// (unreadable or blanked) is matched on Name.
//
// When argv[0] is an interpreter (see interpreters), each rule is also tried on the tool it
// runs: the first non-flag argument, or the module after -m. The lists are in kinds_list.go.
func Classify(p Process) Kind {
	if p.PID == 0 {
		return KindOther
	}
	cmds := []command{{name: baseName(p.Name)}}
	if prog := program(p); prog != "" {
		cmds[0] = command{baseName(prog), p.Argv[1:]}
		if c, ok := cmds[0].unwrap(); ok {
			cmds = append(cmds, c)
		}
	}
	for _, rule := range kindRules {
		for _, c := range cmds {
			if rule.match(c) {
				return rule.kind
			}
		}
	}
	if len(p.Listeners) > 0 {
		return KindServer
	}
	return KindOther
}

// command is a lower-case basename and the arguments after it.
type command struct {
	name string
	args []string
}

var kindRules = []struct {
	kind  Kind
	match func(command) bool
}{
	{KindAgent, func(c command) bool { return agentNames[c.name] }},
	{KindTest, func(c command) bool {
		return testNames[c.name] || strings.HasSuffix(c.name, testSuffix) ||
			len(c.args) > 0 && testCommands[c.name+" "+c.args[0]]
	}},
	{KindWatcher, func(c command) bool {
		if watcherNames[c.name] {
			return true
		}
		for _, a := range c.args {
			if watcherCommands[c.name+" "+a] {
				return true
			}
		}
		return false
	}},
	{KindEditor, func(c command) bool { return editorNames[c.name] || strings.HasSuffix(c.name, editorSuffix) }},
	{KindShell, func(c command) bool { return shellNames[c.name] }},
}

// program is the path of the program p runs as argv[0] gives it, "" when there is none. A
// one-element argv holding a space is either a path with spaces or a title rewritten with
// setproctitle (Chromium's and Electron's children on Linux), the whole command line in one
// string whose last "/" may be a later argument's: its text up to the first space, else the
// whole string, is the program only when its basename is the kernel name or, cut, starts with
// it (as fullName reads it); otherwise there is none and Name stands for it (DEV-168).
func program(p Process) string {
	if len(p.Argv) == 0 {
		return ""
	}
	a := p.Argv[0]
	if len(p.Argv) > 1 || !strings.Contains(a, " ") {
		return a
	}
	first, _, _ := strings.Cut(a, " ")
	for _, s := range []string{first, a} {
		if b := strings.TrimPrefix(path.Base(s), "-"); b == p.Name || len(p.Name) >= commCut && strings.HasPrefix(b, p.Name) {
			return s
		}
	}
	return ""
}

// baseName lower-cases the basename of s and drops a login shell's leading "-".
func baseName(s string) string {
	return strings.ToLower(strings.TrimPrefix(path.Base(s), "-"))
}

// Tool returns the tool an interpreter process runs, for the TUI's `<tool> (<name>)` label
// (spec "Release 1.1", tool labels): when the basename of argv[0] is an interpreter (see
// interpreters; trailing version digits ignored) or a shell of scriptShells, the module after
// -m as written, else the basename of the first argument that is not a flag, case and
// extension kept (`vite` for node_modules/.bin/vite, `server.js`, `manage.py`, `run55.sh`).
// args are the arguments after it, a subslice of p.Argv. ok is false when argv[0] is neither
// or names no tool. It is the argument Classify unwraps, a shell's script aside: both use
// toolArg.
func Tool(p Process) (tool string, args []string, ok bool) {
	prog := program(p)
	if prog == "" {
		return "", nil, false
	}
	rest := p.Argv[1:]
	i, tool, module := toolArg(baseName(prog), rest)
	if i < 0 {
		return "", nil, false
	}
	if !module {
		tool = path.Base(tool)
	}
	return tool, rest[i+1:], true
}

// toolArg returns the index in args of the tool interpreter or shell name runs, the tool as
// written there, and whether it is a module (after -m) rather than a script; -1 when name is
// neither (an interpreter's trailing version digits ignored) or args name no tool. The tool is
// the module after -m (python's also attached, -mpytest: DEV-153), else the first argument
// that is not a flag or one of subcommands. A flag of inlineCodeFlags before it means the
// interpreter runs inline code, which names no tool (DEV-137); a flag of valueFlags skips its
// value too (DEV-138). Other flags taking a separate value are not known, so `node -r x app.js`
// yields x; good enough to find jest or pytest. A shell's flags start with - or +; a cluster
// of short ones holding c (inline code) or s (standard input) names no tool, and one ending in
// o or O takes the next argument as its value (`-eo pipefail`) (DEV-159).
func toolArg(name string, args []string) (i int, tool string, module bool) {
	shell := scriptShells[name]
	name = strings.TrimRight(name, "0123456789.")
	if !interpreters[name] && !shell {
		return -1, "", false
	}
	skip, sub := false, false
	for i, a := range args {
		flag, _, _ := strings.Cut(a, "=")
		switch {
		case skip:
			skip = false
			continue
		case shell && len(a) > 1 && (a[0] == '-' || a[0] == '+') && a[1] != '-':
			if strings.ContainsAny(a, "cs") {
				return -1, "", false
			}
			skip = strings.HasSuffix(a, "o") || strings.HasSuffix(a, "O")
			continue
		case a == "-m" && i+1 < len(args):
			return i + 1, args[i+1], true
		case name == "python" && len(a) > 2 && strings.HasPrefix(a, "-m"):
			return i, a[2:], true
		case inlineCodeFlags[name+" "+flag]:
			return -1, "", false
		case valueFlags[name+" "+a]:
			skip = true
			continue
		case !sub && subcommands[name+" "+a]:
			sub = true
			continue
		case strings.HasPrefix(a, "-"):
			continue
		}
		return i, a, false
	}
	return -1, "", false
}

// unwrap returns the tool an interpreter runs (toolArg) as Classify matches it: a module
// lower-cased, a script by its lower-case basename without a script extension. A shell's
// script is not unwrapped, so the shell keeps its kind (DEV-159).
func (c command) unwrap() (command, bool) {
	i, tool, module := toolArg(c.name, c.args)
	switch {
	case i < 0 || scriptShells[c.name]:
		return command{}, false
	case module:
		return command{strings.ToLower(tool), c.args[i+1:]}, true
	}
	name := baseName(tool)
	if ext := path.Ext(name); scriptExts[ext] {
		name = strings.TrimSuffix(name, ext)
	}
	return command{name, c.args[i+1:]}, true
}
