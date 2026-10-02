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
	if len(p.Argv) > 0 {
		cmds[0] = command{baseName(p.Argv[0]), p.Argv[1:]}
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

// baseName lower-cases the basename of s and drops a login shell's leading "-".
func baseName(s string) string {
	return strings.ToLower(strings.TrimPrefix(path.Base(s), "-"))
}

// unwrap returns the tool an interpreter runs: the module after -m, else the first argument
// that is not a flag, without a script extension. Flags taking a separate value are not
// known, so `node -r x app.js` yields x; good enough to find jest or pytest.
func (c command) unwrap() (command, bool) {
	if !interpreters[strings.TrimRight(c.name, "0123456789.")] {
		return command{}, false
	}
	for i, a := range c.args {
		switch {
		case a == "-m" && i+1 < len(c.args):
			return command{strings.ToLower(c.args[i+1]), c.args[i+2:]}, true
		case strings.HasPrefix(a, "-"):
			continue
		}
		name := baseName(a)
		if ext := path.Ext(name); scriptExts[ext] {
			name = strings.TrimSuffix(name, ext)
		}
		return command{name, c.args[i+1:]}, true
	}
	return command{}, false
}
