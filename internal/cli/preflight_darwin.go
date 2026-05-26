//go:build darwin

package cli

import (
	"errors"
	"os"
)

// checkFUSEPrereqs verifies macFUSE is installed before `veilfs mount`
// daemonizes. Without it the background child's mount fails and the
// parent can only report the opaque "daemon failed to start" message;
// checking here surfaces the real cause and the fix immediately.
func checkFUSEPrereqs() error {
	// macFUSE installs its filesystem bundle here; its absence is the
	// usual reason a mount fails on a fresh macOS machine.
	if _, err := os.Stat("/Library/Filesystems/macfuse.fs"); err != nil {
		return errors.New("macFUSE does not appear to be installed.\n" +
			"  Install it with `brew install --cask macfuse`, then reboot and approve\n" +
			"  the system extension (System Settings -> Privacy & Security).")
	}
	return nil
}
