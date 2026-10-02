package main

import "runtime/debug"

// versionInfo returns the version, commit and commit date to print. Each linker value (set by
// goreleaser with -ldflags -X) wins on its own; one still at its default ("dev", "none",
// "unknown") falls back to the build info the go command embeds: the module version for
// `go install ...@v0.1.0`, and the vcs revision and time for a build inside a git checkout, the
// revision suffixed "-dirty" when the tree had changes. bi nil means no build info.
func versionInfo(version, commit, date string, bi *debug.BuildInfo) (string, string, string) {
	if bi == nil {
		return version, commit, date
	}
	if version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
	var rev, when string
	var modified bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.time":
			when = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if commit == "none" && rev != "" {
		commit = rev
		if modified {
			commit += "-dirty"
		}
	}
	if date == "unknown" && when != "" {
		date = when
	}
	return version, commit, date
}

// buildInfo is debug.ReadBuildInfo, nil when the binary carries none.
func buildInfo() *debug.BuildInfo {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return nil
	}
	return bi
}
