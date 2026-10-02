package main

import (
	"runtime/debug"
	"testing"
)

func TestVersionInfo(t *testing.T) {
	vcs := func(version string, kv ...string) *debug.BuildInfo {
		bi := &debug.BuildInfo{Main: debug.Module{Path: "github.com/dogauzun/devdash", Version: version}}
		for i := 0; i < len(kv); i += 2 {
			bi.Settings = append(bi.Settings, debug.BuildSetting{Key: kv[i], Value: kv[i+1]})
		}
		return bi
	}
	const rev, when = "0123456789abcdef0123456789abcdef01234567", "2026-10-01T12:34:56Z"
	tests := []struct {
		name                string
		version, commit, dt string // the linker values
		bi                  *debug.BuildInfo
		want                [3]string
	}{
		{
			name:    "ldflags set, build info ignored",
			version: "v0.1.0", commit: "abc1234", dt: "2026-09-30T00:00:00Z",
			bi:   vcs("v9.9.9", "vcs.revision", rev, "vcs.time", when, "vcs.modified", "true"),
			want: [3]string{"v0.1.0", "abc1234", "2026-09-30T00:00:00Z"},
		},
		{
			name:    "go install from the module proxy",
			version: "dev", commit: "none", dt: "unknown",
			bi:   vcs("v0.1.0"),
			want: [3]string{"v0.1.0", "none", "unknown"},
		},
		{
			name:    "build in a clean checkout",
			version: "dev", commit: "none", dt: "unknown",
			bi:   vcs("(devel)", "vcs", "git", "vcs.revision", rev, "vcs.time", when, "vcs.modified", "false"),
			want: [3]string{"dev", rev, when},
		},
		{
			name:    "build in a modified checkout",
			version: "dev", commit: "none", dt: "unknown",
			bi:   vcs("(devel)", "vcs.revision", rev, "vcs.time", when, "vcs.modified", "true"),
			want: [3]string{"dev", rev + "-dirty", when},
		},
		{
			name:    "empty module version",
			version: "dev", commit: "none", dt: "unknown",
			bi:   vcs(""),
			want: [3]string{"dev", "none", "unknown"},
		},
		{
			name:    "no build info",
			version: "dev", commit: "none", dt: "unknown",
			bi:   nil,
			want: [3]string{"dev", "none", "unknown"},
		},
		{
			name:    "partial ldflags: version only",
			version: "v0.2.0", commit: "none", dt: "unknown",
			bi:   vcs("v9.9.9", "vcs.revision", rev, "vcs.time", when, "vcs.modified", "true"),
			want: [3]string{"v0.2.0", rev + "-dirty", when},
		},
		{
			name:    "partial ldflags: commit only",
			version: "dev", commit: "abc1234", dt: "unknown",
			bi:   vcs("v0.1.0", "vcs.revision", rev, "vcs.time", when, "vcs.modified", "true"),
			want: [3]string{"v0.1.0", "abc1234", when},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, c, d := versionInfo(tt.version, tt.commit, tt.dt, tt.bi)
			if got := [3]string{v, c, d}; got != tt.want {
				t.Errorf("versionInfo = %q, want %q", got, tt.want)
			}
		})
	}
}
