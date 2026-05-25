package main

import (
	"fmt"
	"os"

	"veilfs/internal/cli"
)

const usage = `veilfs hides files matching a .veilignore pattern from a passthrough FUSE mount.

Usage:
  veilfs mount [-f] [--config FILE] <source> <target>
  veilfs umount <target>

Commands:
  mount   Mount <source> at <target>, applying the .veilignore at the source
          root (or the file given via --config). Runs in the background unless
          -f is supplied. New entries with hidden names are rejected so the
          underlying files cannot be overwritten via the mount.
  umount  Tear down a previously created veilfs mount.
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
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "veilfs: unknown subcommand %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "veilfs: %v\n", err)
		os.Exit(1)
	}
}
