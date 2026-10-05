package model

import (
	"path"
	"slices"
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
		{argv: "npx -p nodemon nodemon server.js", listeners: listen, want: KindWatcher}, // npx -p is --package, not inline code (DEV-138)
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
		// A shell running a script keeps its kind: the script is its label only (DEV-159).
		{argv: "/bin/bash ./run55.sh 4", want: KindShell},
		{argv: "bash ./server.sh", listeners: listen, want: KindShell},
		{argv: "sh node_modules/.bin/jest", want: KindShell},
		{argv: "bash --rcfile rc claude", want: KindShell},
		{argv: "zsh -o pipefail nodemon.zsh", want: KindShell},
		{argv: "fish vitest", want: KindShell},
		// server and other
		{argv: "node server.js", listeners: listen, want: KindServer},
		{argv: "python3 -m http.server", listeners: listen, want: KindServer},
		// -mMODULE is -m MODULE; bun's run and x name no tool (DEV-153).
		{argv: "python3 -mpytest", want: KindTest},
		{argv: "python3 -mhttp.server 8000", listeners: listen, want: KindServer},
		{argv: "bun run vitest", want: KindTest},
		{argv: "bun --watch run nodemon", want: KindWatcher},
		{argv: "bun x jest", want: KindTest},
		{argv: "bun run dev", listeners: listen, want: KindServer},
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

func TestTool(t *testing.T) {
	tests := []struct {
		argv string // space-separated; "" for no argv
		tool string // "" for ok false
		args string // space-separated
	}{
		{argv: "node node_modules/.bin/vite --port 5173", tool: "vite", args: "--port 5173"},
		{argv: "python3 -m uvicorn app:main --reload", tool: "uvicorn", args: "app:main --reload"},
		{argv: "python3.12 -m pytest -q", tool: "pytest", args: "-q"},
		{argv: "node server.js", tool: "server.js"},
		{argv: "python3 manage.py runserver", tool: "manage.py", args: "runserver"},
		{argv: "npx vitest run", tool: "vitest", args: "run"},
		{argv: "/usr/local/bin/node22 --inspect /app/Server.MJS", tool: "Server.MJS"}, // case and extension kept
		{argv: "python -m Http.Server 8000", tool: "Http.Server", args: "8000"},       // the module as written
		{argv: "node -r x app.js", tool: "x", args: "app.js"},                         // the argument Classify reads
		{argv: "node"},
		{argv: "node --inspect"},
		// Inline code is not a tool (DEV-137): the program text follows -c, -e, --eval, -p or --print.
		{argv: "python3 -c from@multiprocessing.spawn@import@spawn_main; --multiprocessing-fork"},
		{argv: "python3 -B -c import@a/b"},
		{argv: "node -e setInterval(()=>{},1e6)"},
		{argv: "node --eval x app.js"},
		{argv: "node --eval=x app.js"},
		{argv: "node -p 1+1"},
		{argv: "bun --print 1"},
		{argv: "python3 -m pytest -c pytest.ini", tool: "pytest", args: "-c pytest.ini"}, // after the tool, -c is its own
		// The flags are each interpreter's own (DEV-138): -p is npx's --package and ruby's loop,
		// -e is python's nothing, and python's -X and -W take the next argument as their value.
		{argv: "npx -p nodemon nodemon server.js", tool: "nodemon", args: "nodemon server.js"},
		{argv: "bunx -p vite vite", tool: "vite", args: "vite"},
		{argv: "npx -c tsc@-p@./tsconfig.json@--watch"},
		{argv: "npx --call=tsc@--watch"},
		{argv: "ruby -p script.rb", tool: "script.rb"},
		{argv: "ruby -e puts@1"},
		{argv: "nodejs --eval x"},
		{argv: "python3 -X dev -W ignore -c from@multiprocessing.spawn@import@spawn_main; --multiprocessing-fork"},
		{argv: "python3 -X dev app.py", tool: "app.py"},
		{argv: "python3 -Wignore app.py", tool: "app.py"},
		{argv: "python3 -m"},
		// python's -m takes its module attached too, a flag of its own for other interpreters;
		// bun's run and x are subcommands, the tool follows them (DEV-153).
		{argv: "python3 -mhttp.server --bind 127.0.0.1 6107", tool: "http.server", args: "--bind 127.0.0.1 6107"},
		{argv: "python3 -mpytest", tool: "pytest"},
		{argv: "python3 -B -mpytest -x", tool: "pytest", args: "-x"},
		{argv: "node -mx app.js", tool: "app.js"},
		{argv: "bun run dev", tool: "dev"},
		{argv: "bun --watch run server.ts", tool: "server.ts"},
		{argv: "bun x vite", tool: "vite"},
		{argv: "bun run vitest --watch", tool: "vitest", args: "--watch"},
		{argv: "bun run --hot ./src/index.ts", tool: "index.ts"},
		{argv: "bun run run", tool: "run"},
		{argv: "bun run"},
		{argv: "bun x"},
		{argv: "bunx run", tool: "run"},
		{argv: "node run dev", tool: "run", args: "dev"},
		// A shell running a script file names it (DEV-159); inline code (-c, also in -lc or -ec),
		// standard input (-s) and a login or interactive shell name none. -o, -O, +o, +O,
		// --rcfile and --init-file take the next argument as their value.
		{argv: "/bin/bash ./run55.sh 4", tool: "run55.sh", args: "4"},
		{argv: "-bash ./guard.sh", tool: "guard.sh"},
		{argv: "sh scripts/dev.sh", tool: "dev.sh"},
		{argv: "/bin/zsh -x build.zsh", tool: "build.zsh"},
		{argv: "dash x.sh", tool: "x.sh"},
		{argv: "ksh x.ksh", tool: "x.ksh"},
		{argv: "fish x.fish", tool: "x.fish"},
		{argv: "bash -o pipefail script.sh", tool: "script.sh"},
		{argv: "bash -eo pipefail script.sh", tool: "script.sh"},
		{argv: "bash +o posix script.sh", tool: "script.sh"},
		{argv: "bash -O extglob +O nullglob script.sh", tool: "script.sh"},
		{argv: "bash +x script.sh", tool: "script.sh"},
		{argv: "bash --rcfile rc script.sh", tool: "script.sh"},
		{argv: "bash --init-file rc script.sh", tool: "script.sh"},
		{argv: "bash --norc -- script.sh", tool: "script.sh"},
		{argv: "bash -m ./script.sh", tool: "script.sh"}, // a shell's -m is job control
		{argv: "sh -c make@dev"},
		{argv: "bash -lc make@dev"},
		{argv: "sh -ec make@dev"},
		{argv: "zsh -o pipefail -c make@dev"},
		{argv: "fish -c make@dev"},
		{argv: "fish --command=make@dev"},
		{argv: "sh -s stable"},
		{argv: "-zsh"},
		{argv: "bash"},
		{argv: "bash -i"},
		{argv: "zsh -l"},
		{argv: "nu script.nu"},
		{argv: "bash5 script.sh"},
		{argv: "/usr/sbin/sshd -D"},
		{argv: "vite --port 5173"},
		{argv: ""},
	}
	for _, tt := range tests {
		argv := strings.Fields(tt.argv)
		for i := range argv {
			argv[i] = strings.ReplaceAll(argv[i], "@", " ") // one argument with spaces, as inline code is
		}
		p := Process{PID: 42, Name: "node", Argv: argv}
		tool, args, ok := Tool(p)
		if ok != (tt.tool != "") || tool != tt.tool || !slices.Equal(args, strings.Fields(tt.args)) {
			t.Errorf("Tool(%q) = %q, %q, %v; want %q, %q, %v", tt.argv, tool, args, ok, tt.tool, strings.Fields(tt.args), tt.tool != "")
		}
	}
}

// TestToolMatchesClassify: Classify matches on the argument Tool names, lower-cased and
// without a script extension, so the two never pick different arguments. A shell's script is
// a label only (DEV-159): TestClassify's shell cases show that Classify does not unwrap it.
func TestToolMatchesClassify(t *testing.T) {
	for _, argv := range []string{
		"node /app/node_modules/.bin/Jest --ci", "node --max-old-space-size=4096 /app/node_modules/jest/bin/jest.js",
		"python3 -m PyTest -x", "npx vitest run", "node /app/node_modules/nodemon/bin/nodemon.js",
		"node -r ts-node/register jest.ts", "python3.12 /usr/local/bin/pytest", "ruby bin/Rails.rb s",
		"node", "node --inspect", "python3 -m", "/usr/sbin/sshd -D", "-zsh",
		"python3 -c import@pytest", "node -e require('jest')", "node --print=x jest",
		"npx -p nodemon nodemon server.js", "python3 -X dev -m pytest", "ruby -p spec.rb",
		"python3 -mPyTest", "bun run Vitest", "bun --watch x jest", "bun run",
	} {
		argv := strings.Fields(strings.ReplaceAll(argv, "@", "\x00"))
		for i := range argv {
			argv[i] = strings.ReplaceAll(argv[i], "\x00", " ")
		}
		tool, args, ok := Tool(Process{PID: 42, Argv: argv})
		c, cok := command{baseName(argv[0]), argv[1:]}.unwrap()
		want := strings.ToLower(tool)
		if ext := path.Ext(want); scriptExts[ext] {
			want = strings.TrimSuffix(want, ext)
		}
		if ok != cok || ok && (c.name != want || !slices.Equal(c.args, args)) {
			t.Errorf("%q: Tool = %q, %q, %v; unwrap = %q, %q, %v", argv, tool, args, ok, c.name, c.args, cok)
		}
	}
}
