// Package cli implements the mount/umount subcommands for veilfs.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"veilfs/internal/ignore"
	"veilfs/internal/vfs"
)

// daemonEnv signals to a child process that it was launched as the
// background half of `veilfs mount`. The child reports mount success or
// failure to its parent via the file descriptor named in daemonReadyFdEnv.
const (
	daemonEnv         = "__VEILFS_DAEMON"
	daemonReadyFdEnv  = "__VEILFS_READY_FD"
)

// Mount is the entry point for `veilfs mount`.
func Mount(args []string) error {
	fs := flag.NewFlagSet("mount", flag.ContinueOnError)
	var configPath string
	var foreground bool
	var debug bool
	var caseMode string
	fs.StringVar(&configPath, "config", "", "path to an alternative ignore file (overrides .veilignore in source)")
	fs.BoolVar(&foreground, "f", false, "run in the foreground instead of daemonizing")
	fs.BoolVar(&debug, "debug", false, "enable FUSE protocol logging")
	fs.StringVar(&caseMode, "case-mode", "auto", "case matching: auto|on|off (auto probes the source filesystem)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: veilfs mount [flags] <source> <target>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return errors.New("mount: expected <source> <target>")
	}

	source, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("source path: %w", err)
	}
	target, err := filepath.Abs(fs.Arg(1))
	if err != nil {
		return fmt.Errorf("target path: %w", err)
	}
	if st, err := os.Stat(source); err != nil || !st.IsDir() {
		return fmt.Errorf("source must be an existing directory: %s", source)
	}
	if st, err := os.Stat(target); err != nil || !st.IsDir() {
		return fmt.Errorf("target must be an existing directory: %s", target)
	}
	if resolvedSource, errA := filepath.EvalSymlinks(source); errA == nil {
		if resolvedTarget, errB := filepath.EvalSymlinks(target); errB == nil {
			if err := checkNotNested(resolvedSource, resolvedTarget); err != nil {
				return err
			}
		}
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

	isDaemonChild := os.Getenv(daemonEnv) == "1"

	if !foreground && !isDaemonChild {
		return daemonize(source, target, ignorePath, configPath != "", debug, caseMode)
	}

	return serve(source, target, ignorePath, debug, isDaemonChild, caseInsensitive)
}

// resolveCaseMode maps the user-facing case-mode flag to a boolean.
// "auto" probes the source filesystem; "on"/"off" are explicit overrides.
// Probe failure falls back to case-sensitive (the more permissive but
// also the more historically common Unix default) and the failure is
// surfaced via stderr so operators can react.
func resolveCaseMode(mode, source string) (bool, error) {
	switch mode {
	case "on", "true", "yes", "insensitive":
		return true, nil
	case "off", "false", "no", "sensitive":
		return false, nil
	case "auto", "":
		ci, err := detectCaseInsensitive(source)
		if err != nil {
			fmt.Fprintf(os.Stderr, "veilfs: case-mode probe failed (%v); defaulting to case-INsensitive matching for safety\n", err)
			return true, nil
		}
		return ci, nil
	default:
		return false, fmt.Errorf("invalid --case-mode %q (want auto|on|off)", mode)
	}
}

// serve performs the actual FUSE mount and blocks until the server stops
// or a termination signal is received. When invoked as a daemon child,
// it signals readiness on the inherited pipe before blocking.
func serve(source, target, ignorePath string, debug, isDaemonChild, caseInsensitive bool) error {
	matcher, err := ignore.NewLiveMatcherWithOptions(ignorePath, source, ignore.Options{
		CaseInsensitive: caseInsensitive,
		Logf:            log.Printf,
	})
	if err != nil {
		notifyParent(isDaemonChild, false)
		return fmt.Errorf("ignore matcher: %w", err)
	}
	if err := matcher.Start(); err != nil {
		notifyParent(isDaemonChild, false)
		return fmt.Errorf("ignore watcher: %w", err)
	}
	defer matcher.Stop()

	srv, err := vfs.Mount(source, target, matcher, vfs.MountOptions{Debug: debug})
	if err != nil {
		notifyParent(isDaemonChild, false)
		return fmt.Errorf("fuse mount: %w", err)
	}

	notifyParent(isDaemonChild, true)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	done := make(chan struct{})
	go func() {
		srv.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		if err := srv.Unmount(); err != nil {
			log.Printf("veilfs: unmount: %v", err)
		}
		<-done
	case <-done:
	}
	return nil
}

// notifyParent writes a single byte to the readiness pipe inherited from
// the parent daemonize call. When isChild is false this is a no-op.
func notifyParent(isChild, ok bool) {
	if !isChild {
		return
	}
	fdStr := os.Getenv(daemonReadyFdEnv)
	fd, err := strconv.Atoi(fdStr)
	if err != nil {
		return
	}
	pipe := os.NewFile(uintptr(fd), "veilfs-ready")
	if pipe == nil {
		return
	}
	defer pipe.Close()
	b := byte('e')
	if ok {
		b = 'k'
	}
	_, _ = pipe.Write([]byte{b})
}

// daemonize re-executes the current binary in the background and waits
// for it to report mount success or failure via a pipe before returning.
func daemonize(source, target, ignorePath string, configExplicit, debug bool, caseMode string) error {
	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("pipe: %w", err)
	}
	defer pr.Close()

	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("executable path: %w", err)
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open /dev/null: %w", err)
	}
	defer devnull.Close()

	childArgs := []string{"veilfs", "mount", "-f"}
	if debug {
		childArgs = append(childArgs, "-debug")
	}
	if configExplicit {
		childArgs = append(childArgs, "--config", ignorePath)
	}
	if caseMode != "" && caseMode != "auto" {
		childArgs = append(childArgs, "--case-mode", caseMode)
	}
	childArgs = append(childArgs, source, target)

	// fd 0,1,2 = /dev/null; fd 3 = write end of readiness pipe.
	procAttr := &os.ProcAttr{
		Dir:   "/",
		Files: []*os.File{devnull, devnull, devnull, pw},
		Env: append(os.Environ(),
			daemonEnv+"=1",
			daemonReadyFdEnv+"=3",
		),
		Sys: &syscall.SysProcAttr{Setsid: true},
	}
	proc, err := os.StartProcess(bin, childArgs, procAttr)
	if err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	if err := pw.Close(); err != nil {
		return fmt.Errorf("close pipe writer: %w", err)
	}

	buf := make([]byte, 1)
	n, err := io.ReadFull(pr, buf)
	if err != nil || n != 1 {
		_ = proc.Release()
		return fmt.Errorf("daemon did not signal readiness: %w", err)
	}
	_ = proc.Release()

	if buf[0] != 'k' {
		return errors.New("daemon failed to start; check syslog for details")
	}
	fmt.Fprintf(os.Stderr, "veilfs: mounted %s on %s\n", source, target)
	return nil
}

// checkNotNested rejects the case where source and target overlap. A
// target inside source would have the mount recursively expose itself,
// and a source inside target would obscure the backing tree behind the
// fresh FUSE mount once the kernel publishes it.
func checkNotNested(source, target string) error {
	if source == target {
		return fmt.Errorf("source and target must differ: %s", source)
	}
	if isAncestor(source, target) {
		return fmt.Errorf("target must not live inside source (source=%s, target=%s)", source, target)
	}
	if isAncestor(target, source) {
		return fmt.Errorf("source must not live inside target (source=%s, target=%s)", source, target)
	}
	return nil
}

// isAncestor reports whether parent is a strict ancestor directory of
// child. Both paths must already be absolute and symlink-resolved.
// The check distinguishes the parent-reference component ".." from
// directory names that merely begin with two dots (e.g. "..mnt"), which
// would otherwise be misclassified as outside the parent tree.
func isAncestor(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == "." || rel == "" {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// Umount is the entry point for `veilfs umount`.
func Umount(args []string) error {
	fs := flag.NewFlagSet("umount", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: veilfs umount <target>")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("umount: expected <target>")
	}
	target, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("target path: %w", err)
	}
	return runUnmount(target)
}

// runUnmount tries the platform-appropriate unmount strategies in order,
// returning the first success or the last error.
func runUnmount(target string) error {
	candidates := unmountCommands(target)
	var lastErr error
	for _, c := range candidates {
		cmd := exec.Command(c[0], c[1:]...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("%s: %v: %s", c[0], err, string(out))
	}
	if lastErr == nil {
		return errors.New("no unmount strategy available for this platform")
	}
	return lastErr
}
