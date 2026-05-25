//go:build linux

package cli

// unmountCommands returns the Linux-specific commands to try when
// unmounting a veilfs target, preferring unprivileged tools.
func unmountCommands(target string) [][]string {
	return [][]string{
		{"fusermount3", "-u", target},
		{"fusermount", "-u", target},
		{"umount", target},
	}
}
