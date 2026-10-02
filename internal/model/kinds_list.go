package model

// Name lists for Classify (spec "Process tree and kinds"). Extend them here; the logic in
// kind.go does not change. Names are lower-case basenames: Classify lower-cases the basename
// of argv[0] (or Name when argv is unknown) and strips the leading "-" of a login shell.

// agentNames: coding agents.
var agentNames = map[string]bool{
	"claude":       true, // Claude Code
	"codex":        true, // OpenAI Codex CLI
	"aider":        true, // Aider
	"gemini":       true, // Gemini CLI
	"cursor-agent": true, // Cursor CLI agent
	"opencode":     true, // opencode
}

// testNames: test runners, matched by basename.
var testNames = map[string]bool{
	"jest":    true, // JavaScript
	"vitest":  true, // JavaScript (Vite)
	"mocha":   true, // JavaScript
	"pytest":  true, // Python
	"py.test": true, // Python, legacy entry point
	"rspec":   true, // Ruby
	"phpunit": true, // PHP
}

// testCommands: "<basename> <argv[1]>" pairs, for tools whose first argument is a subcommand.
var testCommands = map[string]bool{
	"go test":       true, // Go
	"cargo test":    true, // Rust
	"cargo nextest": true, // Rust, cargo-nextest
	"bun test":      true, // Bun
	"deno test":     true, // Deno
	"npm test":      true, // npm script runner
	"pnpm test":     true, // pnpm script runner
	"yarn test":     true, // yarn script runner
	"dotnet test":   true, // .NET
	"mix test":      true, // Elixir
}

// testSuffix: basename suffix of a compiled test binary (`go test` builds pkg.test and runs it).
const testSuffix = ".test"

// watcherNames: file watchers that restart or rebuild on change, matched by basename.
var watcherNames = map[string]bool{
	"nodemon":   true, // Node
	"air":       true, // Go live reload
	"watchexec": true, // generic
	"fswatch":   true, // generic, macOS
	"entr":      true, // generic
	"reflex":    true, // generic (Go)
	"watchman":  true, // Meta's watch service
}

// watcherCommands: "<basename> <arg>" pairs where arg may appear anywhere in the rest of argv.
var watcherCommands = map[string]bool{
	"tsc --watch":     true, // TypeScript compiler in watch mode
	"tsc -w":          true, // same, short flag
	"webpack --watch": true, // webpack
	"esbuild --watch": true, // esbuild
	"rollup --watch":  true, // rollup
	"rollup -w":       true, // same, short flag
	"cargo watch":     true, // Rust, cargo-watch
}

// editorNames: editors and known language servers, matched by basename.
var editorNames = map[string]bool{
	"code":          true, // VS Code
	"code-insiders": true, // VS Code Insiders
	"cursor":        true, // Cursor
	"zed":           true, // Zed
	"subl":          true, // Sublime Text
	"nvim":          true, // Neovim
	"vim":           true, // Vim
	"vi":            true, // vi
	"emacs":         true, // Emacs
	"hx":            true, // Helix
	"nano":          true, // nano
	"gopls":         true, // Go language server
	"tsserver":      true, // TypeScript server
	"rust-analyzer": true, // Rust language server
	"clangd":        true, // C/C++ language server
	"pylsp":         true, // Python language server
	"jdtls":         true, // Java language server
	"sourcekit-lsp": true, // Swift language server
	"zls":           true, // Zig language server
}

// editorSuffix: any basename ending in it is a language server.
const editorSuffix = "-language-server"

// shellNames: interactive and script shells.
var shellNames = map[string]bool{
	"sh":   true, // POSIX sh
	"bash": true, // Bash
	"zsh":  true, // Zsh
	"fish": true, // fish
	"nu":   true, // Nushell
	"dash": true, // Debian sh
	"ksh":  true, // Korn shell
	"tcsh": true, // tcsh
	"csh":  true, // C shell
	"pwsh": true, // PowerShell
}

// interpreters: runtimes and package runners whose first non-flag argument (or the module
// after `-m`) names the real tool, as in `node …/jest.js`, `python -m pytest` or `npx vitest`.
// Trailing version digits are ignored, so python3.12 and node22 match.
var interpreters = map[string]bool{
	"node":   true, // Node.js
	"nodejs": true, // Node.js, Debian name
	"bun":    true, // Bun
	"bunx":   true, // Bun package runner
	"npx":    true, // npm package runner
	"python": true, // Python, any version
	"ruby":   true, // Ruby
}

// scriptExts: extensions stripped from an interpreter's script, so nodemon.js matches nodemon.
var scriptExts = map[string]bool{".js": true, ".cjs": true, ".mjs": true, ".ts": true, ".py": true, ".rb": true}
