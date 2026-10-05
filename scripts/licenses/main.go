// Command licenses writes THIRD_PARTY_LICENSES, the notices the release archives ship for the
// code linked into devdash besides its own (DEV-98): the Go standard library's LICENSE and
// PATENTS, then, sorted by module path, every module `go list -deps ./cmd/devdash` names for
// the four release targets (CGO_ENABLED=0), with the license, NOTICE and PATENTS files at the
// root of its directory in the module cache. Each target's list goes into the union, so a
// darwin-only module (purego) is there too.
//
// Usage, from the repository root (`make licenses`, `make licenses-check`):
//
//	go run ./scripts/licenses          # rewrite THIRD_PARTY_LICENSES
//	go run ./scripts/licenses -check   # exit 1 if it differs from what would be written
//
// The module cache must hold every module; `go list` downloads a missing one.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// The release targets, as in the Makefile's TARGETS and .goreleaser.yaml.
var targets = []string{"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64"}

const pkg = "./cmd/devdash"

const header = `Third-party licenses
====================

devdash is under the MIT License (LICENSE). Its release binaries for darwin and linux
also contain the code below, each part under its own license, reproduced in full.

Generated from ` + "`go list -deps ./cmd/devdash`" + ` for darwin/amd64, darwin/arm64,
linux/amd64 and linux/arm64 by ` + "`make licenses`" + ` (scripts/licenses). Do not edit.
`

// module is one non-standard module the binary links. Version carries the replacement too,
// when go.mod has one (`v1.0.0 => example.com/fork v1.0.1`).
type module struct {
	Path    string
	Version string
	Dir     string
}

// notice is one license, NOTICE or PATENTS file, its text with LF line ends and a final newline.
type notice struct {
	Name string
	Text string
}

// part is one section of the output.
type part struct {
	Title   string
	Notices []notice
}

func main() {
	out := flag.String("o", "THIRD_PARTY_LICENSES", "file to write, or to compare with -check")
	checkOnly := flag.Bool("check", false, "compare with the file instead of writing it; exit 1 if it differs")
	flag.Parse()

	text, err := generate()
	if err != nil {
		fail(err)
	}
	if *checkOnly {
		if err := check(*out, text); err != nil {
			fail(err)
		}
		return
	}
	if err := os.WriteFile(*out, text, 0o644); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "licenses: %v\n", err)
	os.Exit(1)
}

// generate lists the modules for every target and renders their notices after Go's own.
func generate() ([]byte, error) {
	mods := map[string]module{}
	for _, target := range targets {
		goos, goarch, _ := strings.Cut(target, "/")
		cmd := exec.Command("go", "list", "-deps", "-json=Standard,Module", pkg)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
		cmd.Stderr = os.Stderr
		stdout, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go list (%s): %w", target, err)
		}
		if err := parseList(bytes.NewReader(stdout), mods); err != nil {
			return nil, fmt.Errorf("go list (%s): %w", target, err)
		}
	}

	out, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		return nil, fmt.Errorf("go env GOROOT: %w", err)
	}
	std, err := stdNotices(strings.TrimSpace(string(out)))
	if err != nil {
		return nil, fmt.Errorf("standard library: %w", err)
	}
	parts := []part{{Title: "Go standard library", Notices: std}}

	paths := make([]string, 0, len(mods))
	for p := range mods {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		m := mods[p]
		if m.Dir == "" {
			return nil, fmt.Errorf("%s %s: not in the module cache (run go mod download)", m.Path, m.Version)
		}
		notices, err := readNotices(m.Dir)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", m.Path, m.Version, err)
		}
		parts = append(parts, part{Title: m.Path + " " + m.Version, Notices: notices})
	}
	return render(parts), nil
}

// parseList adds to mods every module in a `go list -deps -json` stream, except the standard
// library's packages and the main module's. A module listed again with another version is an
// error: each binary links one version of it.
func parseList(r io.Reader, mods map[string]module) error {
	type mod struct {
		Path    string
		Version string
		Dir     string
		Main    bool
		Replace *mod
	}
	dec := json.NewDecoder(r)
	for {
		var p struct {
			Standard bool
			Module   *mod
		}
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		if p.Standard || p.Module == nil || p.Module.Main {
			continue
		}
		m := module{Path: p.Module.Path, Version: p.Module.Version, Dir: p.Module.Dir}
		if rep := p.Module.Replace; rep != nil {
			m.Version += " => " + strings.TrimSpace(rep.Path+" "+rep.Version)
			if rep.Dir != "" {
				m.Dir = rep.Dir
			}
		}
		if old, ok := mods[m.Path]; ok && old != m {
			return fmt.Errorf("%s listed as %s and as %s", m.Path, old.Version, m.Version)
		}
		mods[m.Path] = m
	}
}

// stdNotices reads the standard library's notices from goroot. Homebrew's GOROOT is
// <cellar>/go/<version>/libexec, with PATENTS there and LICENSE moved one level up (DEV-145):
// a libexec with no license file takes the parent's, but only Go's own BSD license.
func stdNotices(goroot string) ([]notice, error) {
	notices, licensed, err := noticesIn(goroot)
	if err != nil {
		return nil, err
	}
	where := goroot
	if !licensed && filepath.Base(goroot) == "libexec" {
		parent, _, err := noticesIn(filepath.Dir(goroot))
		if err != nil {
			return nil, err
		}
		for _, n := range parent {
			if k := kind(n.Name); k != "NOTICE" && k != "PATENTS" && strings.HasPrefix(n.Text, "Copyright 2009 The Go Authors.\n") {
				notices = append(notices, n)
				licensed = true
			}
		}
		sort.Slice(notices, func(i, j int) bool { return notices[i].Name < notices[j].Name })
		where += ", nor the Go license in " + filepath.Dir(goroot)
	}
	if !licensed {
		return nil, fmt.Errorf("no LICENSE, LICENCE or COPYING file in %s", where)
	}
	return notices, nil
}

// readNotices reads the license, NOTICE and PATENTS files at the root of dir, sorted by name.
// A directory without a license file is an error, so a new module cannot ship without one.
func readNotices(dir string) ([]notice, error) {
	notices, licensed, err := noticesIn(dir)
	if err != nil {
		return nil, err
	}
	if !licensed {
		return nil, fmt.Errorf("no LICENSE, LICENCE or COPYING file in %s", dir)
	}
	return notices, nil
}

// noticesIn reads the license, NOTICE and PATENTS files at the root of dir, sorted by name, and
// says whether one of them is a license file.
func noticesIn(dir string) (notices []notice, licensed bool, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false, err
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	for _, name := range noticeFiles(names) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, false, err
		}
		text := strings.ReplaceAll(string(b), "\r\n", "\n")
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		notices = append(notices, notice{Name: name, Text: text})
		if k := kind(name); k != "NOTICE" && k != "PATENTS" {
			licensed = true
		}
	}
	return notices, licensed, nil
}

// noticeFiles picks the license, NOTICE and PATENTS files from names, sorted.
func noticeFiles(names []string) []string {
	var picked []string
	for _, name := range names {
		if kind(name) != "" {
			picked = append(picked, name)
		}
	}
	sort.Strings(picked)
	return picked
}

// kind is LICENSE, LICENCE, COPYING, NOTICE or PATENTS for a file of that name, in any case,
// bare, with a .md or .txt extension, or with a suffix after a dash (LICENSE-APACHE);
// otherwise it is empty.
func kind(name string) string {
	base := strings.ToUpper(name)
	for _, ext := range []string{".MD", ".TXT"} {
		base = strings.TrimSuffix(base, ext)
	}
	for _, k := range []string{"LICENSE", "LICENCE", "COPYING", "NOTICE", "PATENTS"} {
		if base == k || strings.HasPrefix(base, k+"-") {
			return k
		}
	}
	return ""
}

// render lays out the header, a table of contents and each part's files in order.
func render(parts []part) []byte {
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\nContents:\n\n")
	for _, p := range parts {
		b.WriteString("  " + p.Title + "\n")
	}
	rule := strings.Repeat("=", 80)
	for _, p := range parts {
		fmt.Fprintf(&b, "\n%s\n%s\n%s\n", rule, p.Title, rule)
		for _, n := range p.Notices {
			fmt.Fprintf(&b, "\n--- %s ---\n\n%s", n.Name, n.Text)
		}
	}
	return []byte(b.String())
}

// check compares the file at path with want and names the first line that differs.
func check(path string, want []byte) error {
	const fix = "run `make licenses` and commit the result"
	got, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s is missing: %s", path, fix)
	}
	if err != nil {
		return err
	}
	if bytes.Equal(got, want) {
		return nil
	}
	gotLines := strings.Split(string(got), "\n")
	wantLines := strings.Split(string(want), "\n")
	line := 1
	for line <= len(gotLines) && line <= len(wantLines) && gotLines[line-1] == wantLines[line-1] {
		line++
	}
	return fmt.Errorf("%s is stale (first difference at line %d): %s", path, line, fix)
}
