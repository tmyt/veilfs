//go:build linux

package cli

import (
	"os"
	"os/exec"
	"testing"
)

// TestCheckFUSEPrereqsPasses confirms the preflight succeeds on a host
// that actually has the FUSE device and a fusermount helper. It skips
// rather than fails where those are absent, mirroring the FUSE
// end-to-end tests, so it is meaningful in CI (which installs fuse3) but
// harmless on a bare dev box.
func TestCheckFUSEPrereqsPasses(t *testing.T) {
	if f, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0); err != nil {
		t.Skipf("/dev/fuse not available: %v", err)
	} else {
		_ = f.Close()
	}
	if _, err := exec.LookPath("fusermount3"); err != nil {
		if _, err := exec.LookPath("fusermount"); err != nil {
			t.Skip("no fusermount helper on $PATH")
		}
	}

	if err := checkFUSEPrereqs(); err != nil {
		t.Fatalf("checkFUSEPrereqs() = %v, want nil", err)
	}
}
