package model

import (
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	listen := []Listener{{Proto: "tcp4", Addr: lo, Port: 3000}}
	tests := []struct {
		argv      string // space-separated; "" for no argv
		name      string // Name, used when argv is empty
		listeners []Listener
		want      Kind
	}{
		// agent
		{argv: "claude", want: KindAgent},
		{argv: "/opt/homebrew/bin/codex exec", want: KindAgent},
		{argv: "node /usr/local/bin/claude --resume", want: KindAgent},      // npm shim: shebang runs node on the bin link
		{argv: "node /x/@anthropic-ai/claude-code/cli.js", want: KindOther}, // the script name only; package paths are not read
		// test
		{argv: "go test ./...", want: KindTest},
		{argv: "/tmp/go-build123/b001/model.test -test.v", want: KindTest},
		{argv: "cargo test", want: KindTest},
		{argv: "cargo build", want: KindOther},
		{argv: "go build -o test", want: KindOther}, // test only as argv[1]
		{argv: "jest --watch", want: KindTest},      // test beats watcher
		{argv: "node /app/node_modules/.bin/jest", want: KindTest},
		{argv: "node --max-old-space-size=4096 /app/node_modules/jest/bin/jest.js", want: KindTest},
		{argv: "npx vitest run", want: KindTest},
		{argv: "python -m pytest -x", want: KindTest},
		{argv: "/usr/bin/python3.12 /usr/local/bin/pytest", want: KindTest},
		{argv: "bun test", want: KindTest},
		{argv: "rspec spec/", listeners: listen, want: KindTest}, // test beats server
		// watcher
		{argv: "nodemon server.js", want: KindWatcher},
		{argv: "node /app/node_modules/nodemon/bin/nodemon.js", listeners: listen, want: KindWatcher},
		{argv: "tsc --watch", want: KindWatcher},
		{argv: "tsc -p . -w", want: KindWatcher},
		{argv: "tsc -p .", want: KindOther},
		{argv: "node /app/node_modules/.bin/tsc --watch", want: KindWatcher},
		{argv: "air", want: KindWatcher},
		// editor
		{argv: "nvim main.go", want: KindEditor},
		{argv: "/Applications/Cursor.app/Contents/MacOS/Cursor", want: KindEditor},
		{argv: "gopls -mode=stdio", want: KindEditor},
		{argv: "typescript-language-server --stdio", want: KindEditor},
		{argv: "node /x/node_modules/typescript/lib/tsserver.js", want: KindEditor},
		{argv: "gopls serve", listeners: listen, want: KindEditor}, // editor beats server
		// shell
		{argv: "-zsh", want: KindShell},
		{argv: "/bin/bash -c make dev", want: KindShell},
		{argv: "zsh", listeners: listen, want: KindShell}, // a shell that listens is a shell
		// server and other
		{argv: "node server.js", listeners: listen, want: KindServer},
		{argv: "python3 -m http.server", listeners: listen, want: KindServer},
		{argv: "./bin/api", listeners: listen, want: KindServer},
		{argv: "git status", want: KindOther},
		{argv: "node", want: KindOther},
		{argv: "tmux", want: KindOther},
		// no argv: Name
		{name: "zsh", want: KindShell},
		{name: "rust-analyzer", want: KindEditor},
		{name: "postgres", listeners: listen, want: KindServer},
		{name: "launchd", want: KindOther},
	}
	for _, tt := range tests {
		p := Process{PID: 42, Name: tt.name, Listeners: tt.listeners}
		if tt.argv != "" {
			p.Argv = strings.Fields(tt.argv)
			p.Name = "ignored"
		}
		if got := Classify(p); got != tt.want {
			t.Errorf("Classify(argv %q, name %q, %d listeners) = %v, want %v", tt.argv, tt.name, len(tt.listeners), got, tt.want)
		}
	}
}

func TestClassifyUnknownOwner(t *testing.T) {
	p := Process{Name: "unknown", Listeners: []Listener{{Proto: "tcp4", Addr: any4, Port: 22}}, Unknown: unknownOwner}
	if got := Classify(p); got != KindOther {
		t.Errorf("PID 0 pseudo-process: %v, want other", got)
	}
}
