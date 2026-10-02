package collector

import (
	"os"
	"path/filepath"
	"testing"
)

// kernelWd is the test's working directory as the kernel reports it in /proc/<pid>/cwd or
// PROC_PIDVNODEPATHINFO: with symlinks resolved, where os.Getwd keeps $PWD's spelling (a
// checkout reached through a symlink, or under /tmp on macOS, which is /private/tmp) (DEV-91).
func kernelWd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(wd)
	if err != nil {
		t.Fatal(err)
	}
	return real
}
