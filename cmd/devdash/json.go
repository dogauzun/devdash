package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
)

// cpuGap is the time between the two samples of --json, so cpu_percent is a number.
const cpuGap = 200 * time.Millisecond

// runJSON prints one snapshot as schema v1 JSON (docs/json-schema.md). It samples twice,
// cpuGap apart; timing_ms describes the second sample.
func runJSON(ctx context.Context, o engine.Options, stdout, stderr io.Writer) int {
	first, err := engine.Snapshot(ctx, o)
	if err != nil {
		fmt.Fprintln(stderr, "devdash:", err)
		return 1
	}
	time.Sleep(cpuGap)
	start := time.Now()
	snap, err := engine.SnapshotAfter(ctx, o, first)
	if err != nil {
		fmt.Fprintln(stderr, "devdash:", err)
		return 1
	}
	if err := writeJSON(stdout, snap, time.Since(start)); err != nil {
		fmt.Fprintln(stderr, "devdash:", err)
		return 1
	}
	return 0
}

// Schema v1. Every key is always present; a value that is absent or unreadable is null,
// never "" or 0 (docs/json-schema.md, "Conventions").
type (
	jsonSnapshot struct {
		SchemaVersion int                `json:"schema_version"`
		TakenAt       string             `json:"taken_at"`
		Host          jsonHost           `json:"host"`
		Projects      []jsonProject      `json:"projects"`
		Processes     []jsonProcess      `json:"processes"`
		Containers    []jsonContainer    `json:"containers"`
		Warnings      []jsonWarning      `json:"warnings"`
		TimingMS      map[string]float64 `json:"timing_ms"`
	}
	jsonHost struct {
		OS       string `json:"os"`
		Arch     string `json:"arch"`
		Hostname string `json:"hostname"`
		UID      int    `json:"uid"`
	}
	jsonProject struct {
		ID       string  `json:"id"`
		Root     string  `json:"root"`
		Name     string  `json:"name"`
		Branch   *string `json:"branch"`
		ShortSHA *string `json:"short_sha"`
		Worktree bool    `json:"worktree"`
		MainRepo *string `json:"main_repo"`
	}
	jsonProcess struct {
		PID        int            `json:"pid"`
		PPID       *int           `json:"ppid"`
		StartTime  *string        `json:"start_time"`
		UID        *int           `json:"uid"`
		User       *string        `json:"user"`
		Name       string         `json:"name"`
		Argv       []string       `json:"argv"`
		Cwd        *string        `json:"cwd"`
		CPUPercent *float64       `json:"cpu_percent"`
		RSSBytes   *uint64        `json:"rss_bytes"`
		Listeners  []jsonListener `json:"listeners"`
		Kind       string         `json:"kind"`
		Project    *string        `json:"project"`
		Container  *string        `json:"container"`
		Unknown    []string       `json:"unknown"`
	}
	jsonListener struct {
		Proto string `json:"proto"`
		Addr  string `json:"addr"`
		Port  uint16 `json:"port"`
	}
	jsonContainer struct {
		ID             string     `json:"id"`
		Name           string     `json:"name"`
		Image          string     `json:"image"`
		State          string     `json:"state"`
		ComposeProject *string    `json:"compose_project"`
		ComposeService *string    `json:"compose_service"`
		Ports          []jsonPort `json:"ports"`
	}
	jsonPort struct {
		HostIP        *string `json:"host_ip"`
		HostPort      uint16  `json:"host_port"`
		ContainerPort uint16  `json:"container_port"`
		Proto         string  `json:"proto"`
	}
	jsonWarning struct {
		Code  string `json:"code"`
		Count int    `json:"count"`
		Hint  string `json:"hint"`
	}
)

// writeJSON encodes s as schema v1; total is the wall time of the sample, timing_ms.total.
func writeJSON(w io.Writer, s model.Snapshot, total time.Duration) error {
	out := jsonSnapshot{
		SchemaVersion: s.SchemaVersion,
		TakenAt:       utc(s.TakenAt),
		Host:          jsonHost{s.Host.OS, s.Host.Arch, s.Host.Hostname, s.Host.UID},
		Projects:      []jsonProject{},
		Processes:     []jsonProcess{},
		Containers:    []jsonContainer{},
		Warnings:      []jsonWarning{},
		TimingMS:      map[string]float64{"total": ms(total)},
	}
	for _, p := range s.Projects {
		out.Projects = append(out.Projects, jsonProject{p.ID, p.Root, p.Name, str(p.Branch), str(p.ShortSHA), p.Worktree, str(p.MainRepo)})
	}
	for _, p := range s.Processes {
		out.Processes = append(out.Processes, process(p))
	}
	for _, c := range s.Containers {
		jc := jsonContainer{c.ID, c.Name, c.Image, c.State, str(c.ComposeProject), str(c.ComposeService), []jsonPort{}}
		for _, m := range c.Ports {
			var ip *string
			if m.HostIP.IsValid() {
				ip = str(m.HostIP.String())
			}
			jc.Ports = append(jc.Ports, jsonPort{ip, m.HostPort, m.ContainerPort, m.Proto})
		}
		out.Containers = append(out.Containers, jc)
	}
	for _, x := range s.Warnings {
		out.Warnings = append(out.Warnings, jsonWarning{x.Code, x.Count, x.Hint})
	}
	for k, d := range s.Timing {
		out.TimingMS[k] = ms(d)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func process(p model.Process) jsonProcess {
	unknown := p.Unknown
	j := jsonProcess{
		PID:       p.PID,
		Name:      p.Name,
		Listeners: []jsonListener{},
		Kind:      p.Kind.String(),
		Project:   str(p.ProjectID),
		Container: str(p.ContainerID),
	}
	if p.PID != 0 { // the unknown-owner pseudo-process has no parent, start, owner or user
		j.PPID, j.UID, j.StartTime, j.User = &p.PPID, &p.UID, str(utc(p.StartTime)), str(p.User)
	}
	if unknown&model.FieldArgv == 0 {
		j.Argv = append([]string{}, p.Argv...) // known and empty is [], unknown is null
	}
	if unknown&model.FieldCwd == 0 {
		j.Cwd = str(p.Cwd)
	}
	if math.IsNaN(p.CPUPercent) { // CPU unreadable, or not in the first sample: no rate
		unknown |= model.FieldCPU
	} else {
		pct := math.Round(p.CPUPercent*100) / 100
		j.CPUPercent = &pct
	}
	if unknown&model.FieldMem == 0 {
		j.RSSBytes = &p.RSSBytes
	}
	for _, l := range p.Listeners {
		j.Listeners = append(j.Listeners, jsonListener{l.Proto, l.Addr.String(), l.Port})
	}
	j.Unknown = unknown.Names()
	return j
}

// str is nil for "", so an absent string is null, never "".
func str(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// utc formats t as RFC 3339 in UTC with its full precision, "" for the zero time.
func utc(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// ms is d in milliseconds, rounded to the microsecond.
func ms(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}
