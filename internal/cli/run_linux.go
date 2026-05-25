//go:build linux

package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"veilfs/internal/ignore"
	"veilfs/internal/vfs"
)

// runChildEnv marks a process as the in-namespace Stage 1 child. It is a
// light guard against accidental direct invocation of __run_child; the
// real protection is that the mount syscalls below only succeed inside
// the user namespace Stage 0 creates.
const runChildEnv = "__VEILFS_RUN_CHILD"

// childSetupFailExit is the exit status Stage 1 uses when it cannot set
// up the veiled view (mirrors the coreutils convention of 125 for "the
// tool itself failed before running the command").
const childSetupFailExit = 125

// runVeiled (Stage 0) re-executes veilfs as __run_child inside a fresh
// user + mount namespace. The caller's uid/gid are mapped to themselves
// (identity) so the command ultimately runs as the real user, not as
// root-in-namespace. To still mount FUSE — which the identity mapping's
// non-zero euid would otherwise lose at execve — Stage 1 is granted
// CAP_SYS_ADMIN as an ambient capability that survives execve; it drops
// that capability again before launching the command. It forwards
// stdio, waits for the command to finish, and propagates its exit code.
func runVeiled(cfg runConfig) error {
	// Terminal signals reach the whole foreground process group, which
	// includes this wrapper. Catch (and discard) them so Ctrl-C / Ctrl-\
	// does not tear down the FUSE server out from under a command that
	// means to handle the signal itself. signal.Notify installs a real
	// handler (not SIG_IGN), so the disposition resets to SIG_DFL across
	// the execve chain and the command still receives the signal.
	catchAndDiscard(syscall.SIGINT, syscall.SIGQUIT)

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("executable path: %w", err)
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("status pipe: %w", err)
	}
	defer pr.Close()

	childArgs := []string{
		"__run_child",
		"--source", cfg.source,
		"--config", cfg.ignorePath,
		"--orig-cwd", cfg.origCwd,
		fmt.Sprintf("--case-insensitive=%t", cfg.caseInsensitive),
		fmt.Sprintf("--debug=%t", cfg.debug),
		fmt.Sprintf("--keep-cwd=%t", cfg.keepCwd),
	}
	if cfg.cacheRaw != "" {
		childArgs = append(childArgs, "--cache-timeout", cfg.cacheRaw)
	}
	if len(cfg.command) > 0 {
		childArgs = append(childArgs, "--")
		childArgs = append(childArgs, cfg.command...)
	}

	proc := exec.Command(exe, childArgs...)
	proc.Stdin = os.Stdin
	proc.Stdout = os.Stdout
	proc.Stderr = os.Stderr
	proc.Env = append(os.Environ(), runChildEnv+"=1")
	proc.ExtraFiles = []*os.File{pw} // becomes fd 3 in the child
	uid, gid := os.Getuid(), os.Getgid()
	proc.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		// Identity-map the caller so the command runs as the real user
		// inside the namespace (uid is preserved, not flattened to 0).
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: uid, HostID: uid, Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: gid, HostID: gid, Size: 1},
		},
		GidMappingsEnableSetgroups: false, // unprivileged userns requires setgroups=deny
		// With a non-zero euid, capabilities would be cleared at execve;
		// raise CAP_SYS_ADMIN as an ambient capability so Stage 1 can
		// still mount FUSE. Stage 1 clears it before running the command.
		AmbientCaps: []uintptr{unix.CAP_SYS_ADMIN},
	}

	if err := proc.Start(); err != nil {
		pw.Close()
		if errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("veilfs run needs unprivileged user namespaces, which appear disabled on this host.\n"+
				"  Enable them (e.g. `sudo sysctl -w kernel.unprivileged_userns_clone=1`) or use the Docker path in the README.\n"+
				"  underlying error: %w", err)
		}
		return fmt.Errorf("veilfs run: starting namespace child: %w", err)
	}
	// Close the parent's copy of the write end so the read sees EOF once
	// the child closes its end.
	pw.Close()

	status, _ := io.ReadAll(pr)
	waitErr := proc.Wait()

	if len(status) == 0 || status[0] != 'K' {
		// Setup failed in the child; it has already exited. Surface the
		// reported message (the child stays silent to avoid double print).
		msg := "failed to set up the veiled mount (no detail reported)"
		if len(status) > 1 {
			msg = string(status[1:])
		}
		return fmt.Errorf("veilfs run: %s", msg)
	}

	// Setup succeeded; the child's exit status is the command's.
	if waitErr == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		return &ExitError{Code: exitCodeFromExitError(ee)}
	}
	return fmt.Errorf("veilfs run: waiting for command: %w", waitErr)
}

// RunChild (Stage 1) runs inside the new user+mount namespace. It mounts
// veilfs in place over the source directory, then launches the command
// as a child that inherits the veiled view. It reports setup success or
// failure on fd 3 before blocking on the command.
func RunChild(args []string) error {
	status := os.NewFile(3, "veilfs-run-status")
	report := func(b byte, msg string) {
		if status == nil {
			return
		}
		_, _ = status.Write(append([]byte{b}, msg...))
		_ = status.Close()
		status = nil
	}
	fail := func(err error) error {
		report('E', err.Error())
		return &ExitError{Code: childSetupFailExit}
	}

	if os.Getenv(runChildEnv) != "1" {
		return fail(errors.New("__run_child is internal to `veilfs run`"))
	}

	// Survive terminal signals while waiting on the command (see the
	// note in runVeiled); the command itself, after the execve below,
	// receives them with the default disposition.
	catchAndDiscard(syscall.SIGINT, syscall.SIGQUIT)

	fs := flag.NewFlagSet("__run_child", flag.ContinueOnError)
	var source, configPath, origCwd, cacheRaw string
	var caseInsensitive, debug, keepCwd bool
	fs.StringVar(&source, "source", "", "")
	fs.StringVar(&configPath, "config", "", "")
	fs.StringVar(&origCwd, "orig-cwd", "", "")
	fs.StringVar(&cacheRaw, "cache-timeout", "", "")
	fs.BoolVar(&caseInsensitive, "case-insensitive", false, "")
	fs.BoolVar(&debug, "debug", false, "")
	fs.BoolVar(&keepCwd, "keep-cwd", false, "")

	childLeft, command := splitAtDoubleDash(args)
	if err := fs.Parse(childLeft); err != nil {
		return fail(fmt.Errorf("parse child args: %w", err))
	}

	var cacheTimeout time.Duration
	if cacheRaw != "" {
		d, err := parseCacheTimeout(cacheRaw)
		if err != nil {
			return fail(err)
		}
		cacheTimeout = d
	}

	// Detach our mount namespace from the host's propagation so nothing
	// we do leaks back out (and host events don't perturb us).
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fail(fmt.Errorf("make / private: %w", err))
	}

	// Bind the source to a private backing path so veilfs keeps an
	// unshadowed handle to the real tree after we mount over <source>.
	backing, err := os.MkdirTemp("", "veilfs-run-")
	if err != nil {
		return fail(fmt.Errorf("create backing dir: %w", err))
	}
	// Reclaim the backing mountpoint on every exit path, including the
	// setup-failure returns below; without this each failed run would
	// leave an empty /tmp/veilfs-run-* directory behind. A synchronous
	// unmount completes before the rmdir (a lazy MNT_DETACH would leave
	// the directory briefly busy and the rmdir would fail); fall back to
	// MNT_DETACH only if the synchronous unmount cannot proceed.
	defer func() {
		if err := syscall.Unmount(backing, 0); err != nil {
			_ = syscall.Unmount(backing, syscall.MNT_DETACH)
		}
		_ = os.Remove(backing)
	}()
	if err := syscall.Mount(source, backing, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fail(fmt.Errorf("bind %s -> %s: %w", source, backing, err))
	}
	_ = syscall.Mount("", backing, "", syscall.MS_REC|syscall.MS_PRIVATE, "")

	// The matcher must read the ignore file via the backing path, not
	// through <source> (which is about to become the FUSE mount and
	// would recurse).
	backingIgnore := translateIntoBacking(configPath, source, backing)
	matcher, err := ignore.NewLiveMatcherWithOptions(backingIgnore, backing, ignore.Options{
		CaseInsensitive: caseInsensitive,
		Logf:            log.Printf,
	})
	if err != nil {
		return fail(fmt.Errorf("ignore matcher: %w", err))
	}
	if err := matcher.Start(); err != nil {
		return fail(fmt.Errorf("ignore watcher: %w", err))
	}
	defer matcher.Stop()

	srv, err := vfs.Mount(backing, source, matcher, vfs.MountOptions{
		Debug:        debug,
		CacheTimeout: cacheTimeout,
		DirectMount:  true,
	})
	if err != nil {
		return fail(fmt.Errorf("fuse mount over %s: %w", source, err))
	}

	// Setup done — tell Stage 0 the mount is live before we hand control
	// to the (possibly long-lived, interactive) command.
	report('K', "")

	cmdDir := source
	if keepCwd && origCwd != "" {
		cmdDir = origCwd
	}

	// The command must not inherit the CAP_SYS_ADMIN that Stage 1 holds
	// only in order to mount. Clearing the ambient and inheritable sets
	// is enough: with no file capabilities and a non-zero euid, the
	// command's effective/permitted sets come out empty after execve.
	// We deliberately keep Stage 1's own permitted/effective so the
	// deferred teardown can still unmount the backing. Capabilities are
	// per-thread and the command inherits the forking thread's sets, so
	// pin this goroutine and fork exec.Command from the same thread.
	//
	// This is the extent of veilfs run's privilege handling: it does not
	// try to stop a command that actively wants to escape the veil (e.g.
	// by reading the backing bind via /proc or /tmp). veilfs is not a
	// sandbox; for that, run it inside one (see README).
	runtime.LockOSThread()
	if err := clearInheritedCaps(); err != nil {
		return fail(fmt.Errorf("clear inherited capabilities before exec: %w", err))
	}

	argv := resolveCommand(command)
	// Resolve the command through the veiled view (PATH and relative
	// names must see the veil, not the pre-mount source), returning an
	// absolute path so the child does no further lookup.
	bin, lookErr := lookPathIn(cmdDir, argv[0])
	if lookErr != nil {
		fmt.Fprintf(os.Stderr, "veilfs run: %v\n", lookErr)
		// Shell convention: 126 = found but not executable, 127 = not found.
		if errors.Is(lookErr, os.ErrPermission) {
			return &ExitError{Code: 126}
		}
		return &ExitError{Code: 127}
	}

	c := exec.Command(bin, argv[1:]...)
	c.Dir = cmdDir
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	runErr := c.Run()

	// Settle the FUSE server explicitly; the deferred cleanup above
	// reclaims the backing mountpoint directory.
	_ = srv.Unmount()

	if runErr == nil {
		return &ExitError{Code: 0}
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		return &ExitError{Code: exitCodeFromExitError(ee)}
	}
	// Found by lookPathIn but could not be executed (ENOEXEC, a
	// non-executable file, ...): 126, matching shell convention.
	fmt.Fprintf(os.Stderr, "veilfs run: %v\n", runErr)
	return &ExitError{Code: 126}
}

// lookPathIn resolves a command name as seen from dir (the veiled
// working directory), so PATH entries and relative names resolve
// through the veil rather than Stage 1's pre-mount cwd. It returns an
// absolute path and restores the process cwd to "/" — outside the veil
// — so the subsequent unmount is not blocked by our working directory.
func lookPathIn(dir, name string) (string, error) {
	if err := os.Chdir(dir); err != nil {
		return "", fmt.Errorf("chdir %s: %w", dir, err)
	}
	bin, err := exec.LookPath(name)
	_ = os.Chdir("/")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(bin) {
		bin = filepath.Join(dir, bin)
	}
	return bin, nil
}

// catchAndDiscard installs a no-op handler for the given signals so this
// wrapper process does not take the default action (termination) when a
// terminal signal is delivered to the whole foreground process group.
// Using signal.Notify (a real handler) rather than signal.Ignore matters:
// an ignored (SIG_IGN) disposition is inherited across execve, which
// would leave the launched command unable to see Ctrl-C; a handler is
// reset to SIG_DFL on execve instead.
func catchAndDiscard(sigs ...os.Signal) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, sigs...)
	go func() {
		for range ch {
		}
	}()
}

// exitCodeFromExitError maps an *exec.ExitError to a shell-style exit
// status, translating signal termination to 128+signal (e.g. SIGINT ->
// 130) rather than the -1 that ExitCode() reports for signaled processes.
func exitCodeFromExitError(ee *exec.ExitError) int {
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ee.ExitCode()
}

// clearInheritedCaps empties the calling thread's ambient and
// inheritable capability sets, just before the command is forked, so the
// command inherits no capabilities: with no file capabilities and a
// non-zero euid, an empty ambient set yields an empty effective/permitted
// set after execve, and clearing inheritable removes the CAP_SYS_ADMIN
// bit Go left there when it raised the ambient capability.
//
// It intentionally preserves the thread's permitted/effective sets so
// the deferred teardown can still unmount the backing (the forked
// command does not inherit those across execve, so keeping them does
// not leak capability to the command). The bounding set is left alone:
// dropping it needs CAP_SETPCAP and is unnecessary since the command has
// nothing to raise into effect.
func clearInheritedCaps() error {
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fmt.Errorf("clear ambient caps: %w", err)
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return fmt.Errorf("read capability sets: %w", err)
	}
	data[0].Inheritable = 0
	data[1].Inheritable = 0
	if err := unix.Capset(&hdr, &data[0]); err != nil {
		return fmt.Errorf("clear inheritable caps: %w", err)
	}
	return nil
}

// translateIntoBacking rewrites a path that lives under source so it
// resolves through the unshadowed backing bind instead. Paths outside
// source (e.g. an absolute --config elsewhere) are returned unchanged.
func translateIntoBacking(p, source, backing string) string {
	rel, err := filepath.Rel(source, p)
	if err != nil {
		return p
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return filepath.Join(backing, rel)
}
