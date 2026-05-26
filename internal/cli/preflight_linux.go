//go:build linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// checkFUSEPrereqs verifies the kernel device and userspace helper that
// `veilfs mount` needs before it daemonizes. Because the real mount
// happens in a background child, a missing prerequisite would otherwise
// surface only as the opaque "daemon failed to start" message, forcing a
// re-run with -f to see the cause. Running this in the parent turns that
// into an immediate, actionable error.
func checkFUSEPrereqs() error {
	// The kernel FUSE device must exist and be openable. In containers
	// it is commonly absent unless passed through with --device /dev/fuse.
	if f, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0); err != nil {
		return fmt.Errorf("the FUSE device /dev/fuse is not available (%v).\n"+
			"  Load the kernel module with `sudo modprobe fuse`, or, inside Docker,\n"+
			"  pass it through with `--device /dev/fuse`.", err)
	} else {
		_ = f.Close()
	}

	// go-fuse mounts via the fusermount helper (one of these names).
	if _, err := exec.LookPath("fusermount3"); err == nil {
		return nil
	}
	if _, err := exec.LookPath("fusermount"); err == nil {
		return nil
	}
	return errors.New("the fusermount helper was not found on $PATH.\n" +
		"  Install the FUSE userspace tools: Debian/Ubuntu `sudo apt-get install fuse3`,\n" +
		"  Fedora/RHEL `sudo dnf install fuse3`, Alpine `apk add fuse3`.")
}
