package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNoticeFiles(t *testing.T) {
	names := []string{
		"go.mod", "README.md", "license.go", "LICENSE_test.go", "licenses",
		"PATENTS", "LICENSE.txt", "NOTICE", "COPYING", "LICENSE-APACHE", "Licence.md", "notice.txt",
	}
	got := noticeFiles(names)
	want := []string{"COPYING", "LICENSE-APACHE", "LICENSE.txt", "Licence.md", "NOTICE", "PATENTS", "notice.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("noticeFiles = %q, want %q", got, want)
	}
}

func TestParseList(t *testing.T) {
	// Two targets' `go list -deps -json` output, concatenated: std and the main module are
	// skipped, a module seen by both targets is listed once, and a replacement is kept.
	stream := `
{"ImportPath": "fmt", "Standard": true}
{"ImportPath": "vendor/golang.org/x/net/route", "Standard": true}
{"ImportPath": "example.com/b/pkg", "Module": {"Path": "example.com/b", "Version": "v1.2.0", "Dir": "/mod/b@v1.2.0"}}
{"ImportPath": "example.com/a", "Module": {"Path": "example.com/a", "Version": "v0.1.0", "Dir": "/mod/a@v0.1.0"}}
{"ImportPath": "github.com/dogauzun/devdash/cmd/devdash", "Module": {"Path": "github.com/dogauzun/devdash", "Main": true, "Dir": "/src"}}
{"ImportPath": "fmt", "Standard": true}
{"ImportPath": "example.com/b", "Module": {"Path": "example.com/b", "Version": "v1.2.0", "Dir": "/mod/b@v1.2.0"}}
{"ImportPath": "example.com/c", "Module": {"Path": "example.com/c", "Version": "v1.0.0", "Dir": "/fork/c",
  "Replace": {"Path": "example.com/fork/c", "Version": "v1.0.1", "Dir": "/fork/c"}}}
`
	mods := map[string]module{}
	if err := parseList(strings.NewReader(stream), mods); err != nil {
		t.Fatal(err)
	}
	want := map[string]module{
		"example.com/a": {Path: "example.com/a", Version: "v0.1.0", Dir: "/mod/a@v0.1.0"},
		"example.com/b": {Path: "example.com/b", Version: "v1.2.0", Dir: "/mod/b@v1.2.0"},
		"example.com/c": {Path: "example.com/c", Version: "v1.0.0 => example.com/fork/c v1.0.1", Dir: "/fork/c"},
	}
	if !reflect.DeepEqual(mods, want) {
		t.Errorf("parseList = %+v, want %+v", mods, want)
	}
}

func TestParseListRefusesTwoVersions(t *testing.T) {
	// One binary links one version of a module; two would mean the targets disagree on go.mod.
	stream := `
{"ImportPath": "example.com/a", "Module": {"Path": "example.com/a", "Version": "v0.1.0", "Dir": "/mod/a@v0.1.0"}}
{"ImportPath": "example.com/a", "Module": {"Path": "example.com/a", "Version": "v0.2.0", "Dir": "/mod/a@v0.2.0"}}
`
	if err := parseList(strings.NewReader(stream), map[string]module{}); err == nil {
		t.Fatal("parseList accepted two versions of example.com/a")
	}
}

func TestReadNotices(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "LICENSE"), "MIT License\r\n\r\nCopyright (c) A\r\n")
	write(t, filepath.Join(dir, "NOTICE"), "notice, no final newline")
	write(t, filepath.Join(dir, "main.go"), "package a\n")
	if err := os.Mkdir(filepath.Join(dir, "COPYING"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := readNotices(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []notice{
		{Name: "LICENSE", Text: "MIT License\n\nCopyright (c) A\n"},
		{Name: "NOTICE", Text: "notice, no final newline\n"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("readNotices = %q, want %q", got, want)
	}
}

func TestReadNoticesRefusesNoLicense(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "NOTICE"), "a notice is not a license\n")
	if _, err := readNotices(dir); err == nil {
		t.Fatal("readNotices accepted a module with no license file")
	}
}

const goLicense = "Copyright 2009 The Go Authors.\n\nRedistribution and use in source and binary forms...\n"

func TestStdNotices(t *testing.T) {
	// actions/setup-go and go.dev's archives: LICENSE and PATENTS in GOROOT.
	goroot := t.TempDir()
	write(t, filepath.Join(goroot, "LICENSE"), goLicense)
	write(t, filepath.Join(goroot, "PATENTS"), "Go patents\n")
	want := []notice{{Name: "LICENSE", Text: goLicense}, {Name: "PATENTS", Text: "Go patents\n"}}
	if got, err := stdNotices(goroot); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("stdNotices(go.dev layout) = %q, %v; want %q", got, err, want)
	}

	// Homebrew (DEV-145): GOROOT is <cellar>/go/<version>/libexec with PATENTS, LICENSE one level up.
	goroot = filepath.Join(t.TempDir(), "go", "1.27.1", "libexec")
	if err := os.MkdirAll(goroot, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(goroot, "PATENTS"), "Go patents\n")
	write(t, filepath.Join(goroot, "..", "LICENSE"), goLicense)
	write(t, filepath.Join(goroot, "..", "README.md"), "# go\n")
	if got, err := stdNotices(goroot); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("stdNotices(Homebrew layout) = %q, %v; want %q", got, err, want)
	}

	// A license beside libexec that is not Go's is not taken for Go's.
	write(t, filepath.Join(goroot, "..", "LICENSE"), "MIT License\n\nCopyright (c) Someone\n")
	if got, err := stdNotices(goroot); err == nil {
		t.Errorf("stdNotices took a parent license that is not Go's: %q", got)
	}

	// No license anywhere still fails, naming GOROOT.
	if err := os.Remove(filepath.Join(goroot, "..", "LICENSE")); err != nil {
		t.Fatal(err)
	}
	if _, err := stdNotices(goroot); err == nil || !strings.Contains(err.Error(), goroot) {
		t.Errorf("stdNotices with no license = %v, want an error naming %s", err, goroot)
	}
}

func TestRender(t *testing.T) {
	parts := []part{
		{Title: "Go standard library", Notices: []notice{{Name: "LICENSE", Text: "Go license\n"}}},
		{Title: "example.com/a v0.1.0", Notices: []notice{
			{Name: "LICENSE", Text: "A license\n"},
			{Name: "NOTICE", Text: "A notice\n"},
		}},
	}
	got := string(render(parts))
	rule := strings.Repeat("=", 80)
	want := header + `
Contents:

  Go standard library
  example.com/a v0.1.0

` + rule + `
Go standard library
` + rule + `

--- LICENSE ---

Go license

` + rule + `
example.com/a v0.1.0
` + rule + `

--- LICENSE ---

A license

--- NOTICE ---

A notice
`
	if got != want {
		t.Errorf("render:\n%s\nwant:\n%s", got, want)
	}
}

func TestCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "THIRD_PARTY_LICENSES")
	if err := check(path, []byte("a\nb\n")); err == nil || !strings.Contains(err.Error(), "make licenses") {
		t.Errorf("check on a missing file = %v, want an error naming make licenses", err)
	}

	write(t, path, "a\nb\n")
	if err := check(path, []byte("a\nb\n")); err != nil {
		t.Errorf("check on a current file = %v", err)
	}

	err := check(path, []byte("a\nc\n"))
	if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "make licenses") {
		t.Errorf("check on a stale file = %v, want an error naming line 2 and make licenses", err)
	}
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}
