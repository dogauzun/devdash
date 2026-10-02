package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunExitCodes(t *testing.T) {
	tests := []struct {
		args       []string
		want       int
		wantStdout string
	}{
		{nil, 2, ""},
		{[]string{"version"}, 0, "devdash dev (commit none, built unknown)\n"},
		{[]string{"-h"}, 0, ""},
		{[]string{"--bogus"}, 2, ""},
		{[]string{"nope"}, 2, ""},
		{[]string{"version", "extra"}, 2, ""},
		{[]string{"--json", "version"}, 2, ""},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := run(tt.args, &stdout, &stderr); got != tt.want {
				t.Errorf("exit = %d, want %d (stderr %q)", got, tt.want, stderr.String())
			}
			if stdout.String() != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
		})
	}
}
