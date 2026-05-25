package vfs

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"testing"
	"time"

	"veilfs/internal/ignore"
)

// fuseAvailable reports whether this host has a working FUSE backend.
//
// On macOS we look for the macFUSE bundle. On Linux we require both an
// openable /dev/fuse and a usermode helper (fusermount / fusermount3) in
// PATH, since lacking either makes go-fuse's mount fail at runtime —
// common in unprivileged containers, restricted CI sandboxes, and on
// developer machines without `fuse3` installed.
func fuseAvailable() bool {
	switch runtime.GOOS {
	case "darwin":
		_, err := os.Stat("/Library/Filesystems/macfuse.fs")
		return err == nil
	case "linux":
		fd, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0)
		if err != nil {
			return false
		}
		fd.Close()
		if _, err := exec.LookPath("fusermount3"); err == nil {
			return true
		}
		if _, err := exec.LookPath("fusermount"); err == nil {
			return true
		}
		return false
	default:
		return false
	}
}

// mountFixture spins up a veilfs mount over the given source tree with
// patterns and returns the mount point plus a cleanup function. It calls
// t.Skip when FUSE is not available on the host.
func mountFixture(t *testing.T, source string, patterns string) (string, *ignore.LiveMatcher) {
	return mountFixtureWithOpts(t, source, patterns, MountOptions{})
}

// mountFixtureWithOpts is the same as mountFixture but lets callers tune
// the MountOptions (used to exercise non-default CacheTimeout etc.).
func mountFixtureWithOpts(t *testing.T, source string, patterns string, opts MountOptions) (string, *ignore.LiveMatcher) {
	t.Helper()
	if !fuseAvailable() {
		t.Skip("FUSE backend not installed on this host; skipping E2E test")
	}

	cfg := filepath.Join(source, ignore.IgnoreFileName)
	if err := os.WriteFile(cfg, []byte(patterns), 0o644); err != nil {
		t.Fatalf("write .veilignore: %v", err)
	}

	matcher, err := ignore.NewLiveMatcher(cfg, source, t.Logf)
	if err != nil {
		t.Fatalf("NewLiveMatcher: %v", err)
	}
	if err := matcher.Start(); err != nil {
		t.Fatalf("matcher.Start: %v", err)
	}

	mountPoint := t.TempDir()
	srv, err := Mount(source, mountPoint, matcher, opts)
	if err != nil {
		matcher.Stop()
		t.Fatalf("Mount: %v", err)
	}

	t.Cleanup(func() {
		_ = srv.Unmount()
		srv.Wait()
		matcher.Stop()
	})

	// Give the kernel a beat to register the mount.
	time.Sleep(50 * time.Millisecond)

	return mountPoint, matcher
}

func mkfile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestE2E_Hiding(t *testing.T) {
	src := t.TempDir()
	mkfile(t, filepath.Join(src, "safe.txt"), "ok")
	mkfile(t, filepath.Join(src, "secret.env"), "DB_PASSWORD=topsecret")
	mkfile(t, filepath.Join(src, "secrets", "key.pem"), "private")

	mnt, _ := mountFixture(t, src, "secret.env\nsecrets/\n")

	entries, err := os.ReadDir(mnt)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if slices.Contains(names, "secret.env") {
		t.Errorf("secret.env must not appear in listing, got %v", names)
	}
	if slices.Contains(names, "secrets") {
		t.Errorf("secrets must not appear in listing, got %v", names)
	}
	if slices.Contains(names, ".veilignore") {
		t.Errorf(".veilignore must not appear in listing, got %v", names)
	}
	if !slices.Contains(names, "safe.txt") {
		t.Errorf("safe.txt should appear in listing, got %v", names)
	}

	if _, err := os.Stat(filepath.Join(mnt, "secret.env")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat secret.env: want ErrNotExist, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(mnt, "secrets", "key.pem")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat secrets/key.pem: want ErrNotExist, got %v", err)
	}
}

func TestE2E_WriteProtection(t *testing.T) {
	src := t.TempDir()
	originalSecret := "ORIGINAL_TOP_SECRET"
	secretPath := filepath.Join(src, "secret.env")
	mkfile(t, secretPath, originalSecret)
	mkfile(t, filepath.Join(src, "safe.txt"), "ok")

	mnt, _ := mountFixture(t, src, "secret.env\n")

	// Direct write to hidden name must be denied.
	if err := os.WriteFile(filepath.Join(mnt, "secret.env"), []byte("HIJACKED"), 0o644); err == nil {
		t.Errorf("write to hidden name should fail")
	}

	// The underlying file must be untouched.
	got, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatalf("read source secret: %v", err)
	}
	if string(got) != originalSecret {
		t.Errorf("secret.env source content was modified: got %q want %q", string(got), originalSecret)
	}

	// Rename onto a hidden name must be denied.
	if err := os.Rename(filepath.Join(mnt, "safe.txt"), filepath.Join(mnt, "secret.env")); err == nil {
		t.Errorf("rename to hidden name should fail")
	}

	// safe.txt should still be readable and source intact.
	if _, err := os.Stat(filepath.Join(mnt, "safe.txt")); err != nil {
		t.Errorf("safe.txt should still exist: %v", err)
	}
	got, err = os.ReadFile(secretPath)
	if err != nil || string(got) != originalSecret {
		t.Errorf("secret.env disturbed by rename attempt: err=%v contents=%q", err, string(got))
	}

	// Mkdir onto a hidden name must be denied.
	if err := os.Mkdir(filepath.Join(mnt, "secret.env"), 0o755); err == nil {
		t.Errorf("mkdir over hidden name should fail")
	}

	// Writing a non-hidden file must work normally.
	if err := os.WriteFile(filepath.Join(mnt, "new.txt"), []byte("hello"), 0o644); err != nil {
		t.Errorf("normal write should succeed: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(src, "new.txt")); err != nil || string(data) != "hello" {
		t.Errorf("normal write should be visible at source: err=%v data=%q", err, string(data))
	}
}

func TestE2E_DirOnlyPatternAllowsRenameToFileName(t *testing.T) {
	src := t.TempDir()
	mkfile(t, filepath.Join(src, "safe.txt"), "ok")

	mnt, _ := mountFixture(t, src, "secrets/\n")

	// `secrets/` is directory-only; renaming a regular file to `secrets`
	// must succeed and the resulting file must be visible.
	if err := os.Rename(filepath.Join(mnt, "safe.txt"), filepath.Join(mnt, "secrets")); err != nil {
		t.Fatalf("rename file to dir-only-pattern name: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mnt, "secrets")); err != nil {
		t.Errorf("renamed file should be visible: %v", err)
	}
}

func TestE2E_DirOnlyPatternAllowsSymlinkToDirName(t *testing.T) {
	src := t.TempDir()
	mkfile(t, filepath.Join(src, "real.txt"), "hello")

	mnt, _ := mountFixture(t, src, "secrets/\n")

	// A symlink is never a directory; `secrets/` must not block creating
	// a symlink named `secrets`.
	if err := os.Symlink("real.txt", filepath.Join(mnt, "secrets")); err != nil {
		t.Fatalf("symlink to dir-only-pattern name: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(mnt, "secrets")); err != nil {
		t.Errorf("created symlink should be visible: %v", err)
	}
}

func TestE2E_DirOnlyPatternKeepsSameNamedFileVisible(t *testing.T) {
	src := t.TempDir()
	// Pattern is `secrets/` — only the directory should be hidden. A
	// regular file with the same basename must remain visible and
	// readable; the lookup and listing must agree on that.
	mkfile(t, filepath.Join(src, "secrets"), "this is actually a file, not a dir")
	mkfile(t, filepath.Join(src, "safe.txt"), "ok")

	mnt, _ := mountFixture(t, src, "secrets/\n")

	entries, err := os.ReadDir(mnt)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Contains(names, "secrets") {
		t.Errorf("file named 'secrets' must be visible under dir-only pattern, got %v", names)
	}
	got, err := os.ReadFile(filepath.Join(mnt, "secrets"))
	if err != nil {
		t.Fatalf("read file 'secrets': %v", err)
	}
	if string(got) != "this is actually a file, not a dir" {
		t.Errorf("unexpected contents: %q", string(got))
	}
}

func TestE2E_HotReloadBypassesDentryCache(t *testing.T) {
	// Reproduces the cache-coherency hazard: the kernel keeps positive
	// dentries for EntryTimeout. If we only filter at Lookup time, a
	// path resolved before reload could still be opened/stat'd/read via
	// the cached entry for up to EntryTimeout. Operations on hidden
	// paths must therefore guard themselves regardless of how the inode
	// was first acquired.
	src := t.TempDir()
	mkfile(t, filepath.Join(src, "doomed.txt"), "soon-to-be-hidden")

	mnt, _ := mountFixture(t, src, "# initially nothing\n")

	// Warm the kernel dentry cache by resolving the path while it is
	// still visible.
	if _, err := os.Stat(filepath.Join(mnt, "doomed.txt")); err != nil {
		t.Fatalf("initial stat: %v", err)
	}

	// Now add a rule that hides it.
	cfg := filepath.Join(src, ignore.IgnoreFileName)
	if err := os.WriteFile(cfg, []byte("doomed.txt\n"), 0o644); err != nil {
		t.Fatalf("rewrite .veilignore: %v", err)
	}

	// Give fsnotify a moment to deliver the change. We do *not* wait
	// for EntryTimeout to expire because the whole point is that the
	// cached dentry would otherwise still be honored.
	time.Sleep(200 * time.Millisecond)

	target := filepath.Join(mnt, "doomed.txt")

	if _, err := os.Open(target); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("open while dentry cached: want ENOENT, got %v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat while dentry cached: want ENOENT, got %v", err)
	}
	if err := os.Remove(target); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("unlink while dentry cached: want ENOENT, got %v", err)
	}
}

func TestE2E_CacheTimeoutKeepsStatHotAfterReload(t *testing.T) {
	// The opt-in `CacheTimeout` exposes the FUSE EntryTimeout +
	// AttrTimeout knob. When set, a stat result that was warmed
	// before a .veilignore reload must keep returning success for the
	// configured window (the kernel does not call us back). After the
	// window expires the matcher takes effect again.
	src := t.TempDir()
	mkfile(t, filepath.Join(src, "cached.txt"), "kernel-cached")

	mnt, _ := mountFixtureWithOpts(t, src, "# nothing\n", MountOptions{
		CacheTimeout: 2 * time.Second,
	})

	target := filepath.Join(mnt, "cached.txt")

	if _, err := os.Stat(target); err != nil {
		t.Fatalf("warm-up stat: %v", err)
	}

	// Hide the file via hot reload.
	cfg := filepath.Join(src, ignore.IgnoreFileName)
	if err := os.WriteFile(cfg, []byte("cached.txt\n"), 0o644); err != nil {
		t.Fatalf("rewrite .veilignore: %v", err)
	}
	time.Sleep(150 * time.Millisecond)

	// Within the cache window the kernel must still answer stat from
	// its own cache rather than calling into veilfs. This is the
	// secrecy-vs-throughput trade-off the flag is for.
	if _, err := os.Stat(target); err != nil {
		t.Errorf("stat within cache window: want cached success, got %v", err)
	}
}

func TestE2E_HotReloadHidesCachedDirectoryAndBlocksWritesUnderIt(t *testing.T) {
	// After a hot reload hides a directory, both reading its contents
	// (readdir on a cached dentry) and writing children through that
	// cached parent must fail.
	src := t.TempDir()
	dirPath := filepath.Join(src, "vault")
	if err := os.MkdirAll(dirPath, 0o755); err != nil {
		t.Fatalf("mkdir vault: %v", err)
	}
	mkfile(t, filepath.Join(dirPath, "inside.txt"), "kept inside")

	mnt, _ := mountFixture(t, src, "# initially empty\n")

	// Resolve and open the directory while it is visible so its dentry
	// is cached by the kernel.
	cached, err := os.Open(filepath.Join(mnt, "vault"))
	if err != nil {
		t.Fatalf("open vault: %v", err)
	}
	cached.Close()

	// Add a rule hiding the directory.
	cfg := filepath.Join(src, ignore.IgnoreFileName)
	if err := os.WriteFile(cfg, []byte("vault/\n"), 0o644); err != nil {
		t.Fatalf("write .veilignore: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	if _, err := os.ReadDir(filepath.Join(mnt, "vault")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("readdir on hidden dir via cached dentry: want ENOENT, got %v", err)
	}
	if err := os.WriteFile(filepath.Join(mnt, "vault", "smuggled.txt"), []byte("x"), 0o644); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("write under hidden dir via cached dentry: want ENOENT, got %v", err)
	}
	if err := os.Mkdir(filepath.Join(mnt, "vault", "subdir"), 0o755); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("mkdir under hidden dir via cached dentry: want ENOENT, got %v", err)
	}
}

func TestE2E_HidingHoldsWhenSourceIsItselfASymlink(t *testing.T) {
	// If <source> on the command line is a symlink to the real source
	// directory, the symlink-target guard must still recognize paths
	// inside that real directory as in-scope. Otherwise an alias whose
	// absolute target points to the canonical path could expose hidden
	// content.
	real := t.TempDir()
	mkfile(t, filepath.Join(real, "secret.env"), "topsecret")
	if err := os.Symlink(filepath.Join(real, "secret.env"), filepath.Join(real, "alias")); err != nil {
		t.Fatalf("seed alias: %v", err)
	}

	linkDir := filepath.Join(t.TempDir(), "linked-src")
	if err := os.Symlink(real, linkDir); err != nil {
		t.Fatalf("seed source symlink: %v", err)
	}

	mnt, _ := mountFixture(t, linkDir, "secret.env\n")

	if _, err := os.Readlink(filepath.Join(mnt, "alias")); err == nil {
		t.Errorf("symlink target inside symlinked source: want error, got nil")
	}
	if _, err := os.ReadFile(filepath.Join(mnt, "alias")); err == nil {
		t.Errorf("read through alias: want error, got nil")
	}
}

func TestE2E_SymlinkChainIntoHiddenSourceBlocked(t *testing.T) {
	// Visible intermediate hop: alias -> hop -> secret.env (hidden).
	// Following alias must still fail because EvalSymlinks resolves
	// the whole chain and lands on hidden content.
	src := t.TempDir()
	mkfile(t, filepath.Join(src, "secret.env"), "topsecret")
	if err := os.Symlink(filepath.Join(src, "secret.env"), filepath.Join(src, "hop")); err != nil {
		t.Fatalf("seed hop symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(src, "hop"), filepath.Join(src, "alias")); err != nil {
		t.Fatalf("seed alias symlink: %v", err)
	}

	mnt, _ := mountFixture(t, src, "secret.env\n")

	if _, err := os.Readlink(filepath.Join(mnt, "alias")); err == nil {
		t.Errorf("transitive readlink: want error, got nil")
	}
	if _, err := os.ReadFile(filepath.Join(mnt, "alias")); err == nil {
		t.Errorf("transitive open: want error, got nil")
	}
}

func TestE2E_SymlinkToSourceRootBlocked(t *testing.T) {
	// A symlink whose absolute target is the source root (or any
	// directory inside source) is a kernel-side bypass channel: walking
	// alias/hidden.env reaches the underlying file directly, ignoring
	// the matcher. Pre-existing symlinks like that must not be
	// followable through the mount.
	src := t.TempDir()
	mkfile(t, filepath.Join(src, "secret.env"), "topsecret")
	if err := os.Symlink(src, filepath.Join(src, "alias")); err != nil {
		t.Fatalf("seed root symlink: %v", err)
	}

	mnt, _ := mountFixture(t, src, "secret.env\n")

	if _, err := os.Readlink(filepath.Join(mnt, "alias")); err == nil {
		t.Errorf("readlink at root symlink: want error, got nil")
	}
	if _, err := os.ReadFile(filepath.Join(mnt, "alias", "secret.env")); err == nil {
		t.Errorf("walking alias/secret.env: want error, got nil")
	}
}

func TestE2E_SymlinkCreationToSourceRootBlocked(t *testing.T) {
	src := t.TempDir()
	mkfile(t, filepath.Join(src, "secret.env"), "topsecret")

	mnt, _ := mountFixture(t, src, "secret.env\n")

	if err := os.Symlink(src, filepath.Join(mnt, "alias")); err == nil {
		t.Errorf("symlink to source root via mount: want error, got nil")
	}
}

func TestE2E_SymlinkEscapingIntoHiddenSourceBlocked(t *testing.T) {
	// A pre-existing symlink whose absolute target lands on a hidden
	// path inside the source tree must not be followable through the
	// mount. The kernel would otherwise resolve the target against the
	// host filesystem and reach the backing file directly.
	src := t.TempDir()
	mkfile(t, filepath.Join(src, "secret.env"), "topsecret")
	// Create the escaping symlink directly on disk.
	absTarget := filepath.Join(src, "secret.env")
	if err := os.Symlink(absTarget, filepath.Join(src, "alias")); err != nil {
		t.Fatalf("seed symlink: %v", err)
	}

	mnt, _ := mountFixture(t, src, "secret.env\n")

	// Readlink through the mount must refuse to reveal a target that
	// would expose the hidden file.
	got, err := os.Readlink(filepath.Join(mnt, "alias"))
	if err == nil {
		t.Errorf("readlink through escaping symlink: want error, got %q", got)
	}
	// Following it must also fail (the kernel calls Readlink under
	// the hood).
	if _, err := os.ReadFile(filepath.Join(mnt, "alias")); err == nil {
		t.Errorf("reading through escaping symlink: want error, got nil")
	}
}

func TestE2E_SymlinkCreationToHiddenTargetBlocked(t *testing.T) {
	src := t.TempDir()
	mkfile(t, filepath.Join(src, "secret.env"), "topsecret")

	mnt, _ := mountFixture(t, src, "secret.env\n")

	if err := os.Symlink(filepath.Join(src, "secret.env"), filepath.Join(mnt, "alias")); err == nil {
		t.Errorf("creating symlink pointing at hidden source: want error, got nil")
	}
}

func TestE2E_HardLinkBlockedAfterHotReload(t *testing.T) {
	// link(oldpath, newpath) must fail if oldpath has been hidden by a
	// hot reload, even when the kernel still has a cached positive
	// dentry for it. Otherwise an agent could re-expose a hidden file's
	// contents by linking it under a non-matching name.
	src := t.TempDir()
	mkfile(t, filepath.Join(src, "soon-hidden.bin"), "secret")

	mnt, _ := mountFixture(t, src, "# initially empty\n")

	// Warm the dentry cache for the source.
	if _, err := os.Stat(filepath.Join(mnt, "soon-hidden.bin")); err != nil {
		t.Fatalf("initial stat: %v", err)
	}

	// Hide it.
	cfg := filepath.Join(src, ignore.IgnoreFileName)
	if err := os.WriteFile(cfg, []byte("soon-hidden.bin\n"), 0o644); err != nil {
		t.Fatalf("write .veilignore: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	// Hard linking it under a different name must fail.
	err := os.Link(filepath.Join(mnt, "soon-hidden.bin"), filepath.Join(mnt, "alias.bin"))
	if err == nil {
		t.Errorf("hard link to hidden source: expected error, got nil")
	}
	// And the new name must not appear.
	if _, err := os.Stat(filepath.Join(mnt, "alias.bin")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("alias should not exist: %v", err)
	}
}

func TestE2E_OpenFileRevokedOnHotReload(t *testing.T) {
	// A descriptor opened while the path was visible must stop serving
	// reads once .veilignore is updated to hide it. Without this,
	// hot-reload can leak content through still-open fds.
	src := t.TempDir()
	target := filepath.Join(src, "soon-hidden.txt")
	mkfile(t, target, "secret-after-reload")

	mnt, _ := mountFixture(t, src, "# initially empty\n")

	fd, err := os.Open(filepath.Join(mnt, "soon-hidden.txt"))
	if err != nil {
		t.Fatalf("initial open: %v", err)
	}
	defer fd.Close()

	// Sanity: read works while still visible.
	buf := make([]byte, 32)
	n, err := fd.Read(buf)
	if err != nil || string(buf[:n]) != "secret-after-reload" {
		t.Fatalf("baseline read: n=%d err=%v data=%q", n, err, string(buf[:n]))
	}

	// Hide the path via hot reload.
	cfg := filepath.Join(src, ignore.IgnoreFileName)
	if err := os.WriteFile(cfg, []byte("soon-hidden.txt\n"), 0o644); err != nil {
		t.Fatalf("rewrite .veilignore: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	// Rewind to make the read fresh; it must now fail.
	if _, err := fd.Seek(0, 0); err != nil {
		t.Fatalf("seek: %v", err)
	}
	n, err = fd.Read(buf)
	if err == nil {
		t.Errorf("read through open fd after reload: expected error, got n=%d data=%q", n, string(buf[:n]))
	}
}

func TestE2E_OpenFdRevokedAfterRenameThenReload(t *testing.T) {
	// Open a file, rename it, then add the new name to .veilignore.
	// The already-open handle must stop serving reads — without
	// per-call path resolution, the guard would keep checking the old
	// name and let the agent read post-hide content.
	src := t.TempDir()
	mkfile(t, filepath.Join(src, "a.txt"), "soon-hidden-content")

	mnt, _ := mountFixture(t, src, "# initially empty\n")

	f, err := os.Open(filepath.Join(mnt, "a.txt"))
	if err != nil {
		t.Fatalf("open a.txt: %v", err)
	}
	defer f.Close()

	// Rename within the mount.
	if err := os.Rename(filepath.Join(mnt, "a.txt"), filepath.Join(mnt, "b.txt")); err != nil {
		t.Fatalf("rename a->b: %v", err)
	}

	// Now hide the new name.
	cfg := filepath.Join(src, ignore.IgnoreFileName)
	if err := os.WriteFile(cfg, []byte("b.txt\n"), 0o644); err != nil {
		t.Fatalf("write .veilignore: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	if _, err := f.Seek(0, 0); err != nil {
		t.Fatalf("seek: %v", err)
	}
	buf := make([]byte, 64)
	if n, err := f.Read(buf); err == nil {
		t.Errorf("read after rename+reload: expected error, got n=%d data=%q", n, string(buf[:n]))
	}
}

func TestE2E_LockingAndCopyFileRangePreserved(t *testing.T) {
	// Wrapping LoopbackFile in guardedFile must not strip POSIX
	// locking or copy_file_range capability — those are baseline
	// behaviors callers expect from a loopback mount.
	src := t.TempDir()
	mkfile(t, filepath.Join(src, "a.txt"), "hello world")
	mkfile(t, filepath.Join(src, "b.txt"), "")

	mnt, _ := mountFixture(t, src, "# nothing hidden\n")

	// fcntl advisory lock through the mount.
	f, err := os.OpenFile(filepath.Join(mnt, "a.txt"), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open a.txt: %v", err)
	}
	defer f.Close()
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lk); err != nil {
		t.Errorf("F_SETLK through guarded file: %v", err)
	}

	// copy_file_range via Go's io.Copy: this uses the linux
	// copy_file_range syscall when both fds support it. We can not
	// observe the syscall directly from Go, but at minimum the
	// destination contents must match after copy.
	src2, err := os.Open(filepath.Join(mnt, "a.txt"))
	if err != nil {
		t.Fatalf("reopen a.txt: %v", err)
	}
	defer src2.Close()
	dst, err := os.OpenFile(filepath.Join(mnt, "b.txt"), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open b.txt: %v", err)
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src2); err != nil {
		t.Errorf("io.Copy through mount: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(mnt, "b.txt")); err != nil || string(data) != "hello world" {
		t.Errorf("post-copy content: err=%v data=%q", err, string(data))
	}
}

func TestE2E_HotReload(t *testing.T) {
	src := t.TempDir()
	mkfile(t, filepath.Join(src, "safe.txt"), "ok")
	mkfile(t, filepath.Join(src, "later.txt"), "still ok")

	mnt, _ := mountFixture(t, src, "safe.txt\n")

	// Initially safe.txt is hidden, later.txt is visible.
	if _, err := os.Stat(filepath.Join(mnt, "safe.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("safe.txt should be hidden initially: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(mnt, "later.txt")); err != nil {
		t.Errorf("later.txt should be visible initially: %v", err)
	}

	// Update the ignore file to hide later.txt instead.
	cfg := filepath.Join(src, ignore.IgnoreFileName)
	if err := os.WriteFile(cfg, []byte("later.txt\n"), 0o644); err != nil {
		t.Fatalf("rewrite .veilignore: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, errSafe := os.Stat(filepath.Join(mnt, "safe.txt"))
		_, errLater := os.Stat(filepath.Join(mnt, "later.txt"))
		if errSafe == nil && errors.Is(errLater, fs.ErrNotExist) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("hot reload did not take effect within timeout")
}
