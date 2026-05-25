package main

import (
	"errors"
	"fmt"
	"os"
	"runtime/debug"

	"veilfs/internal/cli"
)

// version is the release version, injected at build time via
// -ldflags "-X main.version=...". It stays "dev" for plain `go build`.
var version = "dev"

const usage = `veilfs hides files matching a .veilignore pattern from a passthrough FUSE mount.

Usage:
  veilfs mount [-f] [--config FILE] <source> <target>
  veilfs umount <target>
  veilfs run [flags] [<source>] [-- <command> [args...]]
  veilfs version          (also -v / --version)

Commands:
  mount   Mount <source> at <target>, applying the .veilignore at the source
          root (or the file given via --config). Runs in the background unless
          -f is supplied. New entries with hidden names are rejected so the
          underlying files cannot be overwritten via the mount.
  umount  Tear down a previously created veilfs mount.
  run     (Linux) Launch a command, or your $SHELL, with a veiled view of
          <source> (default: the current directory). Uses user + mount
          namespaces — no Docker or root required. The command and any
          processes it spawns inherit the veiled view; ignored files do
          not exist for them.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "mount":
		err = cli.Mount(os.Args[2:])
	case "umount", "unmount":
		err = cli.Umount(os.Args[2:])
	case "run":
		err = cli.Run(os.Args[2:])
	case "__run_child":
		// Internal: the in-namespace stage of `veilfs run`. Not listed
		// in usage; only ever invoked by veilfs re-execing itself.
		err = cli.RunChild(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, usage)
		return
	case "-v", "--version", "version":
		fmt.Fprintln(os.Stdout, versionString())
		return
	default:
		fmt.Fprintf(os.Stderr, "veilfs: unknown subcommand %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}

	// A command launched by `veilfs run` propagates its own exit status
	// verbatim, with no veilfs-level error message.
	var exitErr *cli.ExitError
	if errors.As(err, &exitErr) {
		os.Exit(exitErr.Code)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "veilfs: %v\n", err)
		os.Exit(1)
	}
}

// versionString renders the build version: the injected `version` (a
// release tag in CI, "dev" otherwise), with the VCS revision appended
// when the binary was built with version-control stamping (the default
// for a local `go build`).
func versionString() string {
	v := version
	if info, ok := debug.ReadBuildInfo(); ok {
		var rev string
		var dirty bool
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if rev != "" {
			if len(rev) > 12 {
				rev = rev[:12]
			}
			if dirty {
				rev += "-dirty"
			}
			return fmt.Sprintf("veilfs %s (%s)", v, rev)
		}
	}
	return "veilfs " + v
}
