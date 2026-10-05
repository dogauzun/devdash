package model

import (
	"testing"
	"time"
)

func TestProjectLabel(t *testing.T) {
	for _, tt := range []struct {
		p    Project
		want string
	}{
		{Project{Name: "shop", Branch: "main"}, "shop @ main"},
		{Project{Name: "shop", Branch: "feat/login", Worktree: true, MainRepo: "/code/shop"}, "shop @ feat/login (worktree)"},
		{Project{Name: "shop", ShortSHA: "1a2b3c4"}, "shop @ 1a2b3c4"},
		{Project{Name: "shop", Worktree: true}, "shop (worktree)"},
		{Project{Name: "shop", Branch: "main", Here: true}, "shop @ main"}, // (here) is the TUI's, not the label's
	} {
		if got := tt.p.Label(); got != tt.want {
			t.Errorf("%+v: Label() = %q, want %q", tt.p, got, tt.want)
		}
	}
}

// TestProcessCommand: an absolute argv[0] shows by its basename, except a one-element argv
// holding spaces, a title rewritten with setproctitle on Linux (DEV-146).
func TestProcessCommand(t *testing.T) {
	for _, tt := range []struct {
		argv []string
		want string
	}{
		{nil, ""},
		{[]string{"uvicorn", "app:main"}, "uvicorn app:main"},
		{[]string{"/usr/sbin/sshd", "-D"}, "sshd -D"},
		{[]string{"/usr/sbin/sshd"}, "sshd"},
		{[]string{"node_modules/.bin/vite", "--port", "5173"}, "node_modules/.bin/vite --port 5173"},
		{[]string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "--type=gpu"}, "Google Chrome --type=gpu"},
		{[]string{"/opt/google/chrome/chrome --type=renderer --user-data-dir=/tmp/profile-AbC123 --num-raster-threads=4"},
			"/opt/google/chrome/chrome --type=renderer --user-data-dir=/tmp/profile-AbC123 --num-raster-threads=4"},
	} {
		if got := (Process{Argv: tt.argv}).Command(); got != tt.want {
			t.Errorf("%q: Command() = %q, want %q", tt.argv, got, tt.want)
		}
	}
}

func TestUptime(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second: "0s", 0: "0s", 45 * time.Second: "45s", time.Minute: "1m", 59*time.Minute + 59*time.Second: "59m",
		time.Hour: "1h", 23 * time.Hour: "23h", 24 * time.Hour: "1d", 400 * 24 * time.Hour: "400d",
	} {
		if got := Uptime(d); got != want {
			t.Errorf("Uptime(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "",
		"vite --port 5173":       "vite --port 5173",
		"naïve 日本":               "naïve 日本",
		"a\x1b]0;pwned\x07b":     "a?]0;pwned?b",
		"tab\there\nnl":          "tab?here?nl",
		"del\x7f c1\u009b":       "del? c1?",
		"bidi\u202eevil":         "bidi?evil",
		"bad\xffutf8":            "bad?utf8",
		"nbsp\u00a0and\u2028sep": "nbsp?and?sep",
		"\u200bzw\u202e":         "?zw?",        // a zero-width space is not printable either
		"\ufffd":                 "\ufffd",      // a real U+FFFD is printable; an invalid byte is not
		"nul\x00 csi\x9b2J":      "nul? csi?2J", // a lone 0x9b is a C1 CSI as a raw byte
		"\x1b[31mred\x1b[0m":     "?[31mred?[0m",
	} {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLocation(t *testing.T) {
	main := &Project{ID: "a", Root: "/code/shop", Here: true}
	wt := &Project{ID: "b", Root: "/code/shop-login", Worktree: true, MainRepo: "/code/shop"}
	wt2 := &Project{ID: "c", Root: "/code/shop-pay", Worktree: true, MainRepo: "/code/shop"}
	other := &Project{ID: "d", Root: "/code/blog"}
	bare := &Project{ID: "e", Root: "/code/api-main", Worktree: true, CommonDir: "/code/api.git"}
	bare2 := &Project{ID: "f", Root: "/code/api-topic", Worktree: true, CommonDir: "/code/api.git"}
	for _, tt := range []struct {
		name     string
		pr, here *Project
		want     string
	}{
		{"here", main, main, "this repo"},
		{"worktree of here", wt, main, "this repo, other worktree"},
		{"main repository of here", main, wt, "this repo, other worktree"},
		{"sibling worktree", wt2, wt, "this repo, other worktree"},
		{"here is a worktree", wt, wt, "this repo"},
		{"sibling worktree of a bare repository", bare2, bare, "this repo, other worktree"},
		{"other bare repository", bare2, &Project{ID: "g", Root: "/code/web-wt", Worktree: true, CommonDir: "/code/web.git"}, ""},
		{"other repository", other, main, ""},
		{"no project", nil, main, ""},
		{"no here", wt, nil, ""},
	} {
		if got := Location(tt.pr, tt.here); got != tt.want {
			t.Errorf("%s: Location = %q, want %q", tt.name, got, tt.want)
		}
	}
}
