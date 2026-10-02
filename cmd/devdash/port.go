package main

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"text/tabwriter"

	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
)

// runPort answers "who has port N" from one snapshot (no CPU, so one sample): exit 0 and one
// line per listener when found, exit 1 and "free" when nothing listens.
func runPort(ctx context.Context, o engine.Options, port uint16, stdout, stderr io.Writer) int {
	s, err := engine.Snapshot(ctx, o)
	var found bool
	if err == nil {
		found, err = writePort(stdout, s, port)
	}
	switch {
	case err != nil:
		fmt.Fprintln(stderr, "devdash:", err)
		return exitFailed
	case !found:
		return 1
	}
	return 0
}

// writePort prints one line per listener on port: pid, name, project (or -) and bind
// address. A socket bound to both IPv4 and IPv6 (dual-stack ::) is one line; two sockets are
// two lines. It prints "free" and returns false when nothing listens on port.
func writePort(w io.Writer, s model.Snapshot, port uint16) (bool, error) {
	projects := map[string]string{}
	for _, p := range s.Projects {
		projects[p.ID] = p.Name
	}
	hint := "run with sudo to see it"
	for _, x := range s.Warnings {
		if x.Code == "listener_owner_unreadable" {
			hint = x.Hint
		}
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	found := false
	for _, p := range s.Processes {
		for _, l := range p.Listeners {
			if l.Port != port {
				continue
			}
			found = true
			project := projects[p.ProjectID]
			if project == "" {
				project = "-"
			}
			addr := netip.AddrPortFrom(l.Addr, l.Port)
			if p.PID == 0 {
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\towner unknown: %s\n", p.PID, p.Name, project, addr, hint)
			} else {
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", p.PID, p.Name, project, addr)
			}
		}
	}
	if !found {
		_, err := fmt.Fprintln(w, "free")
		return false, err
	}
	return true, tw.Flush()
}
