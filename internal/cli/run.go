package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"veilfs/internal/ignore"
)

// ExitError carries the exit status of the command launched by `veilfs
// run` so main can propagate it verbatim. It is returned up through the
// re-exec chain (command → Stage 1 → Stage 0 → main).
type ExitError struct{ Code int }

func (e *ExitError) Error() string {
	return fmt.Sprintf("command exited with status %d", e.Code)
}

// runConfig is the resolved configuration for a `veilfs run` invocation,
// assembled in Stage 0 and consumed by the platform-specific runVeiled.
type runConfig struct {
	source          string   // absolute path to the source directory
	ignorePath      string   // absolute path to the ignore policy file
	caseInsensitive bool     // resolved from --case-mode
	debug           bool     // --debug
	cacheRaw        string   // original --cache-timeout string ("" if unset)
	keepCwd         bool     // --keep-cwd: do not chdir into the veiled root
	origCwd         string   // the caller's working directory
	command         []string // command argv; empty means $SHELL
}

// Run is the entry point for `veilfs run`. It launches a command (or the
// user's shell) with a veiled view of a source directory, using Linux
// mount + user namespaces so no Docker or root is required. See the
// platform-specific runVeiled for the mechanics.
func Run(args []string) error {
	left, command := splitAtDoubleDash(args)

	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var configPath string
	var debug bool
	var caseMode string
	var keepCwd bool
	var cacheTimeout cacheTimeoutFlag
	fs.StringVar(&configPath, "config", "", "path to an alternative ignore file (overrides .veilignore in source)")
	fs.BoolVar(&debug, "debug", false, "enable FUSE protocol logging")
	fs.StringVar(&caseMode, "case-mode", "auto", "case matching: auto|on|off (auto probes the source filesystem)")
	fs.BoolVar(&keepCwd, "keep-cwd", false, "run the command in the current directory instead of chdir-ing into the veiled source root")
	fs.Var(&cacheTimeout, "cache-timeout", "FUSE entry+attr cache duration (0 = disable). Bare number = seconds (e.g. 2, 0.5); Go duration suffix accepted (e.g. 500ms, 1s, 2m).")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: veilfs run [flags] [<source>] [-- <command> [args...]]")
		fs.PrintDefaults()
	}
	positionals, err := parseInterspersed(fs, left)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if len(positionals) > 1 {
		fs.Usage()
		return fmt.Errorf("run: at most one <source> may be given before -- (got %d)", len(positionals))
	}

	sourceArg := "."
	if len(positionals) == 1 {
		sourceArg = positionals[0]
	}
	source, err2 := filepath.Abs(sourceArg)
	if err2 != nil {
		return fmt.Errorf("source path: %w", err2)
	}
	if st, err := os.Stat(source); err != nil || !st.IsDir() {
		return fmt.Errorf("source must be an existing directory: %s", source)
	}

	ignorePath := configPath
	if ignorePath == "" {
		ignorePath = filepath.Join(source, ignore.IgnoreFileName)
	} else {
		ignorePath, err = filepath.Abs(ignorePath)
		if err != nil {
			return fmt.Errorf("config path: %w", err)
		}
	}

	caseInsensitive, err := resolveCaseMode(caseMode, source)
	if err != nil {
		return err
	}

	origCwd, _ := os.Getwd()

	return runVeiled(runConfig{
		source:          source,
		ignorePath:      ignorePath,
		caseInsensitive: caseInsensitive,
		debug:           debug,
		cacheRaw:        cacheTimeout.raw,
		keepCwd:         keepCwd,
		origCwd:         origCwd,
		command:         command,
	})
}

// parseInterspersed parses fs against args while tolerating flags that
// appear after the positional <source>, e.g. `veilfs run src --keep-cwd`.
// The stdlib flag package stops at the first non-flag argument; this
// loop records that argument as a positional and resumes flag parsing
// after it, so flags on either side of <source> are honored. Returns
// the collected positionals.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		rem := fs.Args()
		if len(rem) == 0 {
			return positionals, nil
		}
		positionals = append(positionals, rem[0])
		rest = rem[1:]
	}
}

// splitAtDoubleDash splits args at the first standalone "--" token. The
// left side carries flags and the optional <source>; the right side is
// the command argv (verbatim, so the command's own flags are never
// interpreted by veilfs). When no "--" is present, command is nil.
func splitAtDoubleDash(args []string) (left, command []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

// resolveCommand returns the command to launch: the user-supplied argv,
// or a single-element $SHELL (falling back to /bin/sh) when none was
// given. Resolved at launch time so it reflects the environment of the
// process that actually execs the command.
func resolveCommand(command []string) []string {
	if len(command) > 0 {
		return command
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	return []string{shell}
}
