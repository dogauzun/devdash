package collector

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestCollectSudo: the warnings are marked as fixed by sudo (DEV-144) only below root, only
// when the root that sudo starts would have CAP_SYS_PTRACE (it is in the bounding set; a
// container's default set drops it, DEV-13), and, for unowned listeners, only when an owner
// may be hidden by permissions: another user's socket, or a denied fd read. An own-uid socket
// left unowned with nothing denied is outside the pid namespace or the kernel's, which root
// cannot see either (DEV-66).
func TestCollectSudo(t *testing.T) {
	const (
		other = 0 // socket uid of another user's listener
		own   = fixUser
	)
	tests := []struct {
		name       string
		euid       int
		sudoPtrace bool
		sockUID    int
		fdDenied   bool // fixDenied's fd/ stays chmodded to 000
		fields     bool // want process_fields_unreadable marked (when it is reported)
		owner      bool // want Result.OwnerSudo
	}{
		{"another user's socket", fixUser, true, other, false, true, true},
		{"own socket, nothing denied", fixUser, true, own, false, true, false},
		{"own socket, fd/ denied", fixUser, true, own, true, true, true},
		{"no CAP_SYS_PTRACE for root", fixUser, false, other, true, false, false},
		{"root", 0, true, other, true, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, denied := copyProc500(t)
			if tt.fdDenied && !denied {
				t.Skip("running as root: chmod does not deny")
			}
			if !tt.fdDenied {
				if err := os.Chmod(filepath.Join(dir, strconv.Itoa(fixDenied), "fd"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// One listener nobody holds, and no others.
			const header = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
			for _, f := range []string{"net/tcp", "net/tcp6"} {
				if err := os.WriteFile(filepath.Join(dir, f), []byte(header), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			addListen(t, dir, 8091, tt.sockUID, 2999)
			c := newLinux(dir)
			c.euid, c.ptrace, c.sudoPtrace = tt.euid, true, tt.sudoPtrace
			res, err := c.Collect(context.Background(), Options{})
			if err != nil {
				t.Fatal(err)
			}
			if res.OwnerSudo != tt.owner {
				t.Errorf("OwnerSudo %v, want %v", res.OwnerSudo, tt.owner)
			}
			for _, w := range res.Warnings {
				if w.Code == "process_fields_unreadable" && w.Sudo != tt.fields {
					t.Errorf("process_fields_unreadable Sudo %v, want %v", w.Sudo, tt.fields)
				}
				if w.Code != "process_fields_unreadable" && w.Sudo {
					t.Errorf("%s marked for sudo", w.Code)
				}
			}
		})
	}
}
