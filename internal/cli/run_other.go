//go:build !linux

package cli

import "errors"

// runVeiled is unsupported off Linux: the implementation relies on Linux
// user + mount namespaces. macOS users should use the Docker path
// documented in the README, or `veilfs mount` directly.
func runVeiled(_ runConfig) error {
	return errors.New("`veilfs run` is only supported on Linux (it uses user + mount namespaces); " +
		"on macOS use the Docker path or `veilfs mount` (see README)")
}

// RunChild only ever runs as the Linux re-exec child; off Linux it is a
// no-op stub so the package builds.
func RunChild(_ []string) error {
	return errors.New("`veilfs run` is only supported on Linux")
}
