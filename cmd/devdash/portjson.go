package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/freeport"
	"github.com/dogauzun/devdash/internal/model"
)

// jsonPortAnswer is the object `port N --json` prints, built from schema v1's process and
// container objects (docs/json-schema.md, "port_answer").
type jsonPortAnswer struct {
	SchemaVersion int             `json:"schema_version"`
	TakenAt       string          `json:"taken_at"`
	Port          uint16          `json:"port"`
	Free          bool            `json:"free"`
	Holders       []jsonProcess   `json:"holders"`
	Containers    []jsonContainer `json:"containers"`
	NextFree      *uint16         `json:"next_free"`
}

// runPortJSON answers "who has port N" as one JSON object, with the exit codes of `port N`:
// 0 when something holds N, 1 when it is free (the object is printed either way), 5 when no
// snapshot could be taken or stdout could not be written. It samples twice, as --json does, so
// each holder is exactly the process object --json would print. When N is taken and below
// 65535, next_free comes from the same search as `free N+1`; a probe that fails leaves it null
// and prints the error on stderr, but the holders are still the answer, so the exit code stays.
func runPortJSON(ctx context.Context, o engine.Options, port uint16, stdout, stderr io.Writer) int {
	snap, _, err := sampleTwice(ctx, o)
	if err != nil {
		fmt.Fprintln(stderr, "devdash:", err)
		return exitFailed
	}
	a := portAnswer(snap, port)
	if !a.Free {
		next, ok, err := freeport.Next(snap, port, probe)
		switch {
		case err != nil:
			fmt.Fprintln(stderr, "devdash: next free:", err)
		case ok:
			a.NextFree = &next
		}
	}
	if err := writePortJSON(stdout, a); err != nil {
		fmt.Fprintln(stderr, "devdash:", err)
		return exitFailed
	}
	if a.Free {
		return 1
	}
	return 0
}

// portAnswer is the port answer for port in s, next_free left null. The holders are the
// processes with a listener on port, in snapshot order, the unknown owner included; the
// containers are those publishing port over tcp on any host address, the same ones that make
// `port N` exit 0 with no socket on N (iptables only). The port is free when there is neither.
func portAnswer(s model.Snapshot, port uint16) jsonPortAnswer {
	a := jsonPortAnswer{
		SchemaVersion: s.SchemaVersion,
		TakenAt:       utc(s.TakenAt),
		Port:          port,
		Holders:       []jsonProcess{},
		Containers:    []jsonContainer{},
	}
	for _, p := range s.Processes {
		for _, l := range p.Listeners {
			if l.Port == port {
				a.Holders = append(a.Holders, process(p))
				break
			}
		}
	}
	for _, c := range s.Containers {
		for _, m := range c.Ports {
			if m.HostPort == port && m.TCP() {
				a.Containers = append(a.Containers, containerObject(c))
				break
			}
		}
	}
	a.Free = len(a.Holders) == 0 && len(a.Containers) == 0
	return a
}

// writePortJSON encodes a as --json encodes a snapshot.
func writePortJSON(w io.Writer, a jsonPortAnswer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(a)
}
