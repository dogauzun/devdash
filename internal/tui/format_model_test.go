package tui

import (
	"testing"
	"time"

	"github.com/dogauzun/devdash/internal/model"
)

// TestFormatMatchesModel pins the dashboard's group header label, uptime and cleaning to the
// model's formatters, which `devdash port N` prints on a terminal: the spec has the port answer
// show the TUI's label and uptime format, so the two must not drift (DEV-121).
func TestFormatMatchesModel(t *testing.T) {
	for _, d := range []time.Duration{-time.Second, 0, 59 * time.Second, time.Minute, 90 * time.Minute, 23*time.Hour + 59*time.Minute, 24 * time.Hour, 400 * 24 * time.Hour} {
		if got, want := uptime(d), model.Uptime(d); got != want {
			t.Errorf("uptime(%v) = %q, model.Uptime = %q", d, got, want)
		}
	}
	for _, p := range []model.Project{
		{ID: "/code/shop", Name: "shop", Branch: "main"},
		{ID: "/wt/shop-login", Name: "shop", Branch: "feat/login", Worktree: true, MainRepo: "/code/shop"},
		{ID: "/code/api", Name: "api", ShortSHA: "1a2b3c4"},
		{ID: "/code/x", Name: "x", Worktree: true},
		{ID: "/code/shop", Name: "shop", Branch: "main", Here: true},
	} {
		r := model.Row{Key: model.RowKey{Header: model.GroupProject, Group: p.ID}, Project: &p}
		if got, want := rowLabel(r), p.Label(); got != want {
			t.Errorf("rowLabel(%+v) = %q, Project.Label = %q", p, got, want)
		}
	}
	for _, s := range []string{"", "vite --port 5173", "naïve 日本", "a\x1b]0;x\x07b", "tab\tnl\n", "del\x7f c1\u009b", "bidi\u202e", "bad\xff", "nbsp\u00a0"} {
		if got, want := clean(s), model.Clean(s); got != want {
			t.Errorf("clean(%q) = %q, model.Clean = %q", s, got, want)
		}
	}
}
