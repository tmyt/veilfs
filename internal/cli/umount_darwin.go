//go:build darwin

package cli

// unmountCommands returns the macOS-specific commands to try when
// unmounting a veilfs target, in order of preference.
func unmountCommands(target string) [][]string {
	return [][]string{
		{"umount", target},
		{"diskutil", "unmount", target},
	}
}
