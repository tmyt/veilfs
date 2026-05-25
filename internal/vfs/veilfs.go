// Package vfs wires the veilfs FUSE filesystem on top of go-fuse's loopback
// node. Most operations are delegated to the embedded LoopbackNode; the
// hooks below enforce two invariants:
//
//  1. Paths matching the active ignore matcher are reported as ENOENT on
//     lookup and dropped from directory listings — they appear not to
//     exist within the mount.
//  2. New entries (or rename destinations) whose names would be hidden by
//     the matcher are rejected with EPERM, preventing an agent from
//     accidentally creating a file whose contents would then be invisible
//     from the mount, or from overwriting a hidden credential by writing
//     to a same-named entry. EPERM is used rather than EACCES because the
//     restriction is a policy decision external to filesystem permissions
//     and cannot be lifted via chmod.
package vfs

import (
	"context"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Matcher is the subset of *ignore.LiveMatcher that vfs depends on.
// It is declared as an interface to make testing and substitution easy.
type Matcher interface {
	Match(rel string, isDir bool) bool
	MatchAny(rel string) bool
	CaseInsensitive() bool
}

// veilNode is the FUSE inode type used throughout the mount. It embeds
// the loopback node by pointer so that the bridge's NodeWrapChilder hook
// can propagate the matcher to every child without re-running the lookup
// path through user code.
type veilNode struct {
	*fs.LoopbackNode
	matcher Matcher
}

// Compile-time interface assertions for the operations we implement.
var (
	_ fs.NodeLookuper        = (*veilNode)(nil)
	_ fs.NodeCreater         = (*veilNode)(nil)
	_ fs.NodeMkdirer         = (*veilNode)(nil)
	_ fs.NodeMknoder         = (*veilNode)(nil)
	_ fs.NodeSymlinker       = (*veilNode)(nil)
	_ fs.NodeLinker          = (*veilNode)(nil)
	_ fs.NodeRenamer         = (*veilNode)(nil)
	_ fs.NodeOpendirHandler  = (*veilNode)(nil)
	_ fs.NodeWrapChilder     = (*veilNode)(nil)
	_ fs.NodeOpener          = (*veilNode)(nil)
	_ fs.NodeGetattrer       = (*veilNode)(nil)
	_ fs.NodeSetattrer       = (*veilNode)(nil)
	_ fs.NodeReadlinker      = (*veilNode)(nil)
	_ fs.NodeUnlinker        = (*veilNode)(nil)
	_ fs.NodeRmdirer         = (*veilNode)(nil)
	_ fs.NodeGetxattrer      = (*veilNode)(nil)
	_ fs.NodeSetxattrer      = (*veilNode)(nil)
	_ fs.NodeRemovexattrer   = (*veilNode)(nil)
	_ fs.NodeListxattrer     = (*veilNode)(nil)
	_ fs.NodeCopyFileRanger  = (*veilNode)(nil)
)

// newRoot builds the root veilNode by delegating to fs.NewLoopbackRoot and
// then swapping the returned LoopbackNode into a veilNode. Because the
// bridge calls WrapChild on child creation, descendants are automatically
// wrapped — we only need to do this once at the root.
//
// The source path is symlink-canonicalized before being handed to
// LoopbackRoot so that downstream comparisons against EvalSymlinks
// results (used by the symlink target guards) align on the real path,
// closing a class of bypass where <source> itself is a symlink.
func newRoot(sourcePath string, matcher Matcher) (fs.InodeEmbedder, error) {
	if canon, err := filepath.EvalSymlinks(sourcePath); err == nil {
		sourcePath = canon
	}
	rootEmb, err := fs.NewLoopbackRoot(sourcePath)
	if err != nil {
		return nil, err
	}
	ln := rootEmb.(*fs.LoopbackNode)
	root := &veilNode{LoopbackNode: ln, matcher: matcher}
	// Replace RootNode so loopback's internal root() helper resolves to
	// the wrapped embedder, keeping path computations consistent.
	ln.RootData.RootNode = root
	return root, nil
}

// relPath returns the path of `name` (a direct child of this node)
// relative to the mount root, using forward slashes.
func (n *veilNode) relPath(name string) string {
	dir := n.Path(n.RootData.RootNode.EmbeddedInode())
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// WrapChild ensures every child inode produced by the embedded loopback
// implementation is itself a veilNode that shares this node's matcher.
func (n *veilNode) WrapChild(ctx context.Context, ops fs.InodeEmbedder) fs.InodeEmbedder {
	ln, ok := ops.(*fs.LoopbackNode)
	if !ok {
		return ops
	}
	return &veilNode{LoopbackNode: ln, matcher: n.matcher}
}

// guardSelf returns ENOENT if this node's path is currently hidden by
// the matcher. The kernel caches positive dentries for EntryTimeout, so
// after a hot reload an already-resolved hidden path could otherwise be
// opened, stat'd, or mutated without triggering another Lookup; this
// guard plugs that race.
//
// Additionally, if the on-disk entry has been swapped to a symlink
// (e.g. by an out-of-band rename), we walk its target chain through
// refuseIfTargetInSource so cached positive dentries cannot be used to
// follow a freshly-introduced bypass.
func (n *veilNode) guardSelf() syscall.Errno {
	rel := n.Path(n.RootData.RootNode.EmbeddedInode())
	if rel == "" {
		return 0
	}
	underlying := filepath.Join(n.RootData.Path, rel)
	var st syscall.Stat_t
	if err := syscall.Lstat(underlying, &st); err != nil {
		return 0
	}
	mode := st.Mode & syscall.S_IFMT
	if n.matcher.Match(rel, mode == syscall.S_IFDIR) {
		return syscall.ENOENT
	}
	if mode == syscall.S_IFLNK {
		target, err := readSymlink(underlying)
		if err == nil {
			var resolved string
			if filepath.IsAbs(target) {
				resolved = filepath.Clean(target)
			} else {
				resolved = filepath.Clean(filepath.Join(filepath.Dir(underlying), target))
			}
			if errno := n.refuseIfTargetInSource(resolved, syscall.ENOENT); errno != 0 {
				return errno
			}
		}
	}
	return 0
}

func readSymlink(path string) (string, error) {
	for sz := 256; ; sz *= 2 {
		buf := make([]byte, sz)
		n, err := syscall.Readlink(path, buf)
		if err != nil {
			return "", err
		}
		if n < sz {
			return string(buf[:n]), nil
		}
	}
}

// guardChild returns ENOENT if the named child of this node is currently
// hidden. Used by Unlink/Rmdir/Rename so cached dentries cannot drive
// operations on entries the matcher now hides.
func (n *veilNode) guardChild(name string) syscall.Errno {
	rel := n.relPath(name)
	underlying := filepath.Join(n.RootData.Path, rel)
	var st syscall.Stat_t
	if err := syscall.Lstat(underlying, &st); err != nil {
		return 0
	}
	isDir := (st.Mode & syscall.S_IFMT) == syscall.S_IFDIR
	if n.matcher.Match(rel, isDir) {
		return syscall.ENOENT
	}
	return 0
}

func (n *veilNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	rel := n.relPath(name)
	// Determine whether the underlying entry is a directory so the matcher
	// can correctly honor patterns that only apply to one kind. Without
	// this, a directory-only rule like `secrets/` would also hide a file
	// named `secrets`, contradicting the listing produced by Readdir.
	underlying := filepath.Join(n.RootData.Path, rel)
	var st syscall.Stat_t
	if err := syscall.Lstat(underlying, &st); err == nil {
		isDir := (st.Mode & syscall.S_IFMT) == syscall.S_IFDIR
		if n.matcher.Match(rel, isDir) {
			return nil, syscall.ENOENT
		}
	}
	return n.LoopbackNode.Lookup(ctx, name, out)
}

func (n *veilNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	if errno := n.guardSelf(); errno != 0 {
		return nil, nil, 0, errno
	}
	rel := n.relPath(name)
	if n.matcher.Match(rel, false) {
		return nil, nil, 0, syscall.EPERM
	}
	inode, fh, ff, errno := n.LoopbackNode.Create(ctx, name, flags, mode, out)
	if errno != 0 {
		return inode, fh, ff, errno
	}
	_ = rel
	return inode, n.wrapFile(fh, inode), ff, 0
}

func (n *veilNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if errno := n.guardSelf(); errno != 0 {
		return nil, errno
	}
	// A new directory is, by definition, a directory; evaluate the pattern
	// with that knowledge so file-only rules cannot block valid mkdirs.
	if n.matcher.Match(n.relPath(name), true) {
		return nil, syscall.EPERM
	}
	return n.LoopbackNode.Mkdir(ctx, name, mode, out)
}

func (n *veilNode) Mknod(ctx context.Context, name string, mode, rdev uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if errno := n.guardSelf(); errno != 0 {
		return nil, errno
	}
	if n.matcher.Match(n.relPath(name), false) {
		return nil, syscall.EPERM
	}
	return n.LoopbackNode.Mknod(ctx, name, mode, rdev, out)
}

func (n *veilNode) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if errno := n.guardSelf(); errno != 0 {
		return nil, errno
	}
	// The symlink itself is never a directory at the syscall level, so a
	// directory-only pattern should not block its creation.
	if n.matcher.Match(n.relPath(name), false) {
		return nil, syscall.EPERM
	}
	// Refuse symlinks whose target would resolve to a hidden file
	// inside source. Without this, an agent could write a symlink that
	// the kernel later dereferences against the underlying tree,
	// bypassing the FUSE filter entirely. Targets that escape source
	// are passed through (veilfs makes no claims about them).
	if errno := n.checkProspectiveSymlinkTarget(name, target); errno != 0 {
		return nil, errno
	}
	return n.LoopbackNode.Symlink(ctx, target, name, out)
}

// checkProspectiveSymlinkTarget validates the target of a not-yet-created
// symlink that would live as `name` under this node. Unlike
// checkSymlinkTarget (used in Readlink), the symlink's source path is
// derived from the parent + name rather than n.Path itself.
func (n *veilNode) checkProspectiveSymlinkTarget(name, target string) syscall.Errno {
	if target == "" {
		return 0
	}
	parentSourcePath := filepath.Join(n.RootData.Path, n.Path(n.RootData.RootNode.EmbeddedInode()))
	var resolved string
	if filepath.IsAbs(target) {
		resolved = filepath.Clean(target)
	} else {
		resolved = filepath.Clean(filepath.Join(parentSourcePath, target))
	}
	return n.refuseIfTargetInSource(resolved, syscall.EPERM)
}

func (n *veilNode) Link(ctx context.Context, target fs.InodeEmbedder, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if errno := n.guardSelf(); errno != 0 {
		return nil, errno
	}
	// Refuse to create a hard link whose source inode is currently
	// hidden. Without this check the kernel can satisfy link() from a
	// cached positive dentry that pre-dates a hot reload, re-exposing
	// the hidden file's contents through the new name.
	if tn, ok := target.(*veilNode); ok {
		if errno := tn.guardSelf(); errno != 0 {
			return nil, errno
		}
	}
	if n.matcher.Match(n.relPath(name), false) {
		return nil, syscall.EPERM
	}
	return n.LoopbackNode.Link(ctx, target, name, out)
}

func (n *veilNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	if errno := n.guardSelf(); errno != 0 {
		return errno
	}
	if errno := n.guardChild(name); errno != 0 {
		return errno
	}
	np, ok := newParent.(*veilNode)
	if !ok {
		return syscall.EXDEV
	}
	if errno := np.guardSelf(); errno != 0 {
		return errno
	}
	// Look up the kind of the source entry so the destination is matched
	// with the correct file/dir interpretation. Without this, a regular
	// file renamed onto a directory-only pattern name would be rejected
	// even though the resulting file would remain visible.
	dest := np.relPath(newName)
	sourcePath := filepath.Join(n.RootData.Path, n.relPath(name))
	var st syscall.Stat_t
	if err := syscall.Lstat(sourcePath, &st); err == nil {
		isDir := (st.Mode & syscall.S_IFMT) == syscall.S_IFDIR
		if n.matcher.Match(dest, isDir) {
			return syscall.EPERM
		}
	} else if n.matcher.MatchAny(dest) {
		// Source is gone or unreadable; defer to LoopbackNode for the
		// natural error, but if any interpretation of the destination
		// would be hidden, refuse early so probing for hidden names via
		// rename does not succeed silently.
		return syscall.EPERM
	}
	return n.LoopbackNode.Rename(ctx, name, newParent, newName, flags)
}

func (n *veilNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if errno := n.guardSelf(); errno != 0 {
		return nil, 0, errno
	}
	fh, ff, errno := n.LoopbackNode.Open(ctx, flags)
	if errno != 0 {
		return fh, ff, errno
	}
	return n.wrapFile(fh, n.EmbeddedInode()), ff, 0
}

// wrapFile wraps a regular-file FileHandle returned by Open/Create with
// a matcher-aware shim. The shim re-evaluates the matcher on every
// data-touching operation against the file's current path (resolved
// via the go-fuse inode tree), so hot reload AND in-mount renames
// both correctly revoke access. We deliberately omit the
// FilePassthroughFder interface from the wrapper so the kernel never
// bypasses us via the FUSE passthrough fast-path.
func (n *veilNode) wrapFile(inner fs.FileHandle, inode *fs.Inode) fs.FileHandle {
	if inner == nil {
		return inner
	}
	return &guardedFile{
		inode:    inode,
		rootData: n.RootData,
		inner:    inner,
		matcher:  n.matcher,
	}
}

func (n *veilNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	if errno := n.guardSelf(); errno != 0 {
		return errno
	}
	return n.LoopbackNode.Getattr(ctx, f, out)
}

func (n *veilNode) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if errno := n.guardSelf(); errno != 0 {
		return errno
	}
	return n.LoopbackNode.Setattr(ctx, f, in, out)
}

func (n *veilNode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	if errno := n.guardSelf(); errno != 0 {
		return nil, errno
	}
	raw, errno := n.LoopbackNode.Readlink(ctx)
	if errno != 0 {
		return raw, errno
	}
	// Defense against symlinks whose target re-enters source via a
	// host-path the kernel would not route through the FUSE mount. If
	// the resolved target lands on a path inside the source tree that
	// the matcher hides, refuse to hand back the link text. Targets
	// that escape source entirely are passed through because they
	// reference content veilfs does not manage.
	if errno := n.checkSymlinkTarget(string(raw)); errno != 0 {
		return nil, errno
	}
	return raw, 0
}

func (n *veilNode) checkSymlinkTarget(target string) syscall.Errno {
	if target == "" {
		return 0
	}
	rel := n.Path(n.RootData.RootNode.EmbeddedInode())
	symlinkSourcePath := filepath.Join(n.RootData.Path, rel)
	var resolved string
	if filepath.IsAbs(target) {
		resolved = filepath.Clean(target)
	} else {
		resolved = filepath.Clean(filepath.Join(filepath.Dir(symlinkSourcePath), target))
	}
	return n.refuseIfTargetInSource(resolved, syscall.ENOENT)
}

// refuseIfTargetInSource walks any symlink chain rooted at `resolved`
// and returns `deny` if the final path lands inside source root and
// either references a directory or matches an ignore rule. It returns
// 0 when the chain terminates outside source or on a visible regular
// file inside source. Failure to follow the chain (cycles, dangling
// links, permission errors) is treated conservatively as deny when the
// last successfully resolved hop sits inside source.
//
// Path containment honors the matcher's case-folding mode so that on
// case-insensitive filesystems a target with mismatched case is still
// recognized as inside source root.
func (n *veilNode) refuseIfTargetInSource(resolved string, deny syscall.Errno) syscall.Errno {
	sourceRoot := filepath.Clean(n.RootData.Path)
	final, evalErr := filepath.EvalSymlinks(resolved)
	if evalErr != nil {
		final = resolved
	}
	relTarget, err := caseAwareRel(sourceRoot, final, n.matcher.CaseInsensitive())
	if err != nil {
		return 0
	}
	if relTarget == ".." || strings.HasPrefix(relTarget, ".."+string(filepath.Separator)) {
		return 0
	}
	if evalErr != nil {
		// Inside source but unresolvable: refuse rather than expose
		// whatever the kernel manages to follow.
		return deny
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(final, &st); err != nil {
		return deny
	}
	isDir := (st.Mode & syscall.S_IFMT) == syscall.S_IFDIR
	if isDir {
		return deny
	}
	if relTarget != "." && relTarget != "" && n.matcher.Match(filepath.ToSlash(relTarget), false) {
		return deny
	}
	return 0
}

func (n *veilNode) Unlink(ctx context.Context, name string) syscall.Errno {
	if errno := n.guardChild(name); errno != 0 {
		return errno
	}
	return n.LoopbackNode.Unlink(ctx, name)
}

func (n *veilNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	if errno := n.guardChild(name); errno != 0 {
		return errno
	}
	return n.LoopbackNode.Rmdir(ctx, name)
}

func (n *veilNode) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	if errno := n.guardSelf(); errno != 0 {
		return 0, errno
	}
	return n.LoopbackNode.Getxattr(ctx, attr, dest)
}

func (n *veilNode) Setxattr(ctx context.Context, attr string, data []byte, flags uint32) syscall.Errno {
	if errno := n.guardSelf(); errno != 0 {
		return errno
	}
	return n.LoopbackNode.Setxattr(ctx, attr, data, flags)
}

func (n *veilNode) Removexattr(ctx context.Context, attr string) syscall.Errno {
	if errno := n.guardSelf(); errno != 0 {
		return errno
	}
	return n.LoopbackNode.Removexattr(ctx, attr)
}

func (n *veilNode) Listxattr(ctx context.Context, dest []byte) (uint32, syscall.Errno) {
	if errno := n.guardSelf(); errno != 0 {
		return 0, errno
	}
	return n.LoopbackNode.Listxattr(ctx, dest)
}

// CopyFileRange overrides the promoted LoopbackNode method so that
// guardedFile-wrapped handles are unwrapped before the loopback code
// runs (which expects raw *LoopbackFile values for its type assertion).
// Without this, copy_file_range(2) on any veilfs mount would fail with
// ENOTSUP.
func (n *veilNode) CopyFileRange(ctx context.Context, fhIn fs.FileHandle, offIn uint64, outInode *fs.Inode, fhOut fs.FileHandle, offOut uint64, length uint64, flags uint64) (uint32, syscall.Errno) {
	if errno := n.guardSelf(); errno != 0 {
		return 0, errno
	}
	rawIn := unwrapGuarded(fhIn)
	if rawIn != fhIn {
		// fhIn was a guardedFile; check whether its path is now hidden.
		if g, ok := fhIn.(*guardedFile); ok && g.hidden() {
			return 0, syscall.ENOENT
		}
	}
	rawOut := unwrapGuarded(fhOut)
	if rawOut != fhOut {
		if g, ok := fhOut.(*guardedFile); ok && g.hidden() {
			return 0, syscall.ENOENT
		}
	}
	return n.LoopbackNode.CopyFileRange(ctx, rawIn, offIn, outInode, rawOut, offOut, length, flags)
}

func unwrapGuarded(fh fs.FileHandle) fs.FileHandle {
	if g, ok := fh.(*guardedFile); ok {
		return g.inner
	}
	return fh
}


// OpendirHandle wraps the loopback directory file handle with a filter
// that drops ignored entries during Readdirent. We must override this
// because the bridge prefers the OpendirHandle code path whenever it is
// implemented, bypassing any Readdir override.
func (n *veilNode) OpendirHandle(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if errno := n.guardSelf(); errno != 0 {
		return nil, 0, errno
	}
	fh, ff, errno := n.LoopbackNode.OpendirHandle(ctx, flags)
	if errno != 0 {
		return nil, 0, errno
	}
	return &filteredDirHandle{
		inner:    fh,
		matcher:  n.matcher,
		inode:    n.EmbeddedInode(),
		rootData: n.RootData,
	}, ff, 0
}

// filteredDirHandle wraps a directory FileHandle and filters out entries
// whose names match the active ignore matcher. Only the subset of file
// handle interfaces needed for directory reads is forwarded; in
// particular Seekdir is deliberately not implemented because filtered
// offsets do not correspond one-to-one with underlying offsets.
type filteredDirHandle struct {
	inner    fs.FileHandle
	matcher  Matcher
	inode    *fs.Inode
	rootData *fs.LoopbackRoot
}

// currentRelDir returns the directory's path relative to source root,
// resolved via the inode tree each call so renames before a stale
// readdir continue to evaluate correctly.
func (h *filteredDirHandle) currentRelDir() string {
	return h.inode.Path(h.rootData.RootNode.EmbeddedInode())
}

var (
	_ fs.FileReaddirenter  = (*filteredDirHandle)(nil)
	_ fs.FileReleasedirer  = (*filteredDirHandle)(nil)
)

func (h *filteredDirHandle) Readdirent(ctx context.Context) (*fuse.DirEntry, syscall.Errno) {
	relDir := h.currentRelDir()
	sourceRoot := h.rootData.Path
	// Re-check whether the directory itself has been hidden since the
	// handle was opened. The path is resolved fresh each call so the
	// check survives renames as well as .veilignore reloads. If the
	// backing path no longer resolves at all (out-of-band rename or
	// removal), we fail closed for the same reason guardedFile does.
	if relDir != "" {
		var st syscall.Stat_t
		if err := syscall.Lstat(filepath.Join(sourceRoot, relDir), &st); err != nil {
			return nil, syscall.ENOENT
		}
		if h.matcher.Match(relDir, true) {
			return nil, syscall.ENOENT
		}
	}
	rd, ok := h.inner.(fs.FileReaddirenter)
	if !ok {
		return nil, syscall.ENOSYS
	}
	for {
		de, errno := rd.Readdirent(ctx)
		if errno != 0 || de == nil {
			return de, errno
		}
		if de.Name == "." || de.Name == ".." {
			return de, 0
		}
		rel := de.Name
		if relDir != "" {
			rel = path.Join(relDir, de.Name)
		}
		isDir := (de.Mode & syscall.S_IFMT) == syscall.S_IFDIR
		if (de.Mode & syscall.S_IFMT) == 0 {
			// Backing filesystem returned DT_UNKNOWN; consult Lstat so
			// directory-only ignore rules still match.
			var st syscall.Stat_t
			if syscall.Lstat(filepath.Join(sourceRoot, rel), &st) == nil {
				isDir = (st.Mode & syscall.S_IFMT) == syscall.S_IFDIR
			}
		}
		if h.matcher.Match(rel, isDir) {
			continue
		}
		return de, 0
	}
}

func (h *filteredDirHandle) Releasedir(ctx context.Context, flags uint32) {
	if rd, ok := h.inner.(fs.FileReleasedirer); ok {
		rd.Releasedir(ctx, flags)
	}
}

// caseAwareRel is filepath.Rel that optionally case-folds both inputs.
// When the underlying filesystem is case-insensitive, two paths that
// differ only in case still refer to the same directory; without this
// helper, containment checks against the source root would miss those
// aliases and could be used as a bypass channel for symlink targets.
func caseAwareRel(parent, child string, caseInsensitive bool) (string, error) {
	if !caseInsensitive {
		return filepath.Rel(parent, child)
	}
	return filepath.Rel(strings.ToLower(parent), strings.ToLower(child))
}

// guardedFile wraps a regular-file FileHandle and re-checks the matcher
// on each data-touching operation against the file's current path. We
// resolve the path through the inode tree (rather than snapshotting
// at open time) so that rename + later .veilignore reload still
// revokes access — the alternative would let an agent escape filtering
// by renaming a file before the matcher catches up.
type guardedFile struct {
	inode    *fs.Inode
	rootData *fs.LoopbackRoot
	inner    fs.FileHandle
	matcher  Matcher
}

var (
	_ fs.FileReader     = (*guardedFile)(nil)
	_ fs.FileWriter     = (*guardedFile)(nil)
	_ fs.FileGetattrer  = (*guardedFile)(nil)
	_ fs.FileSetattrer  = (*guardedFile)(nil)
	_ fs.FileAllocater  = (*guardedFile)(nil)
	_ fs.FileLseeker    = (*guardedFile)(nil)
	_ fs.FileFlusher    = (*guardedFile)(nil)
	_ fs.FileFsyncer    = (*guardedFile)(nil)
	_ fs.FileReleaser   = (*guardedFile)(nil)
	_ fs.FileGetlker    = (*guardedFile)(nil)
	_ fs.FileSetlker    = (*guardedFile)(nil)
	_ fs.FileSetlkwer   = (*guardedFile)(nil)
	_ fs.FileIoctler    = (*guardedFile)(nil)
)

func (g *guardedFile) hidden() bool {
	rootInode := g.rootData.RootNode.EmbeddedInode()
	rel := g.inode.Path(rootInode)
	if rel == "" {
		return false
	}
	underlying := filepath.Join(g.rootData.Path, rel)
	var st syscall.Stat_t
	if err := syscall.Lstat(underlying, &st); err != nil {
		// The backing path no longer resolves. This can happen when
		// the source tree is modified out-of-band: either the file
		// was unlinked while we still hold an fd, or it was renamed
		// (possibly into a name the matcher hides). We fail closed —
		// returning hidden=true so subsequent reads/writes receive
		// ENOENT — because we cannot prove the stale identity is
		// still safe to expose. The cost is that legitimately
		// unlinked-but-open files become unreadable through the
		// mount; in exchange we close a host-side rename bypass.
		return true
	}
	isDir := (st.Mode & syscall.S_IFMT) == syscall.S_IFDIR
	return g.matcher.Match(rel, isDir)
}

func (g *guardedFile) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if g.hidden() {
		return nil, syscall.ENOENT
	}
	r, ok := g.inner.(fs.FileReader)
	if !ok {
		return nil, syscall.ENOSYS
	}
	return r.Read(ctx, dest, off)
}

func (g *guardedFile) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	if g.hidden() {
		return 0, syscall.ENOENT
	}
	w, ok := g.inner.(fs.FileWriter)
	if !ok {
		return 0, syscall.ENOSYS
	}
	return w.Write(ctx, data, off)
}

func (g *guardedFile) Getattr(ctx context.Context, out *fuse.AttrOut) syscall.Errno {
	if g.hidden() {
		return syscall.ENOENT
	}
	ga, ok := g.inner.(fs.FileGetattrer)
	if !ok {
		return syscall.ENOSYS
	}
	return ga.Getattr(ctx, out)
}

func (g *guardedFile) Setattr(ctx context.Context, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if g.hidden() {
		return syscall.ENOENT
	}
	sa, ok := g.inner.(fs.FileSetattrer)
	if !ok {
		return syscall.ENOSYS
	}
	return sa.Setattr(ctx, in, out)
}

func (g *guardedFile) Allocate(ctx context.Context, off uint64, size uint64, mode uint32) syscall.Errno {
	if g.hidden() {
		return syscall.ENOENT
	}
	a, ok := g.inner.(fs.FileAllocater)
	if !ok {
		return syscall.ENOSYS
	}
	return a.Allocate(ctx, off, size, mode)
}

// The operations below are cleanup / housekeeping; they cannot exfiltrate
// data on their own, so we forward them unconditionally rather than
// breaking close/flush semantics during a transient hidden state.

func (g *guardedFile) Lseek(ctx context.Context, off uint64, whence uint32) (uint64, syscall.Errno) {
	if g.hidden() {
		// SEEK_END/SEEK_DATA/SEEK_HOLE would otherwise leak the
		// hidden file's size and hole layout to a caller holding a
		// pre-reload fd.
		return 0, syscall.ENOENT
	}
	l, ok := g.inner.(fs.FileLseeker)
	if !ok {
		return 0, syscall.ENOSYS
	}
	return l.Lseek(ctx, off, whence)
}

func (g *guardedFile) Flush(ctx context.Context) syscall.Errno {
	f, ok := g.inner.(fs.FileFlusher)
	if !ok {
		return 0
	}
	return f.Flush(ctx)
}

func (g *guardedFile) Fsync(ctx context.Context, flags uint32) syscall.Errno {
	f, ok := g.inner.(fs.FileFsyncer)
	if !ok {
		return 0
	}
	return f.Fsync(ctx, flags)
}

func (g *guardedFile) Release(ctx context.Context) syscall.Errno {
	r, ok := g.inner.(fs.FileReleaser)
	if !ok {
		return 0
	}
	return r.Release(ctx)
}

// fcntl advisory locking is forwarded unchanged. We deliberately do
// not guard these because POSIX-locking workloads (SQLite, MTAs, etc.)
// rely on stable lock state across the lifetime of an fd; gating on
// the matcher would surface spurious failures during hot reload while
// the actual data access is already covered by Read/Write/Setattr.

func (g *guardedFile) Getlk(ctx context.Context, owner uint64, lk *fuse.FileLock, flags uint32, out *fuse.FileLock) syscall.Errno {
	l, ok := g.inner.(fs.FileGetlker)
	if !ok {
		return syscall.ENOSYS
	}
	return l.Getlk(ctx, owner, lk, flags, out)
}

func (g *guardedFile) Setlk(ctx context.Context, owner uint64, lk *fuse.FileLock, flags uint32) syscall.Errno {
	l, ok := g.inner.(fs.FileSetlker)
	if !ok {
		return syscall.ENOSYS
	}
	return l.Setlk(ctx, owner, lk, flags)
}

func (g *guardedFile) Setlkw(ctx context.Context, owner uint64, lk *fuse.FileLock, flags uint32) syscall.Errno {
	l, ok := g.inner.(fs.FileSetlkwer)
	if !ok {
		return syscall.ENOSYS
	}
	return l.Setlkw(ctx, owner, lk, flags)
}

// Ioctl is guarded because some commands (FIBMAP / FIEMAP / etc.) can
// extract block-mapping or extent information about the underlying
// file. Forwarding to LoopbackFile keeps legitimate workloads working;
// the hidden() gate prevents post-reload metadata exfiltration.
func (g *guardedFile) Ioctl(ctx context.Context, cmd uint32, arg uint64, input []byte, output []byte) (int32, syscall.Errno) {
	if g.hidden() {
		return 0, syscall.ENOENT
	}
	i, ok := g.inner.(fs.FileIoctler)
	if !ok {
		return 0, syscall.ENOSYS
	}
	return i.Ioctl(ctx, cmd, arg, input, output)
}

// MountOptions controls how Mount behaves. Zero values are sensible
// defaults for veilfs (allow_other off, kernel caches disabled).
type MountOptions struct {
	// Debug enables verbose FUSE protocol logging on stderr.
	Debug bool
	// FsName is reported to userspace via /proc/mounts and `mount`. If
	// empty, "veilfs" is used.
	FsName string
	// CacheTimeout sets BOTH the FUSE EntryTimeout (positive-dentry
	// cache) AND the AttrTimeout (stat cache) on the mount. The
	// default of 0 disables both kernel caches, which forces every
	// path resolution and stat to re-enter veilfs and consult the
	// matcher — that is the secure default.
	//
	// Setting this to a non-zero value trades secrecy for throughput:
	// for the configured window after a .veilignore reload, an
	// already-resolved hidden path can still answer stat / lookup
	// from the kernel's cache. Operators with heavy traversal
	// workloads (`find`, `git status`, build systems) may want a
	// small value (e.g. 200ms–1s) to amortize the per-syscall cost.
	CacheTimeout time.Duration
	// DirectMount makes go-fuse perform the mount via the mount(2)
	// syscall directly instead of invoking the setuid fusermount
	// helper. This is required inside a user namespace (as used by
	// `veilfs run`), where fusermount's setuid bit is neutralized but
	// the namespace's mapped-root process holds CAP_SYS_ADMIN. It
	// implies strict mode: there is no fallback to fusermount, so a
	// mount failure surfaces the real mount(2) error rather than a
	// misleading helper error.
	DirectMount bool
}

// Mount starts a FUSE mount that mirrors sourcePath at mountPoint while
// applying matcher to hide and protect entries. The returned server can
// be used to wait for the mount to terminate or to unmount programmatically.
// Both sourcePath and mountPoint must be absolute paths and must already
// exist on disk.
func Mount(sourcePath, mountPoint string, matcher Matcher, opts MountOptions) (*fuse.Server, error) {
	root, err := newRoot(sourcePath, matcher)
	if err != nil {
		return nil, err
	}
	fsName := opts.FsName
	if fsName == "" {
		fsName = "veilfs"
	}
	// Both timeouts share opts.CacheTimeout. The secure default (0)
	// disables both kernel caches so every operation re-enters veilfs
	// and re-checks the matcher; per-op guards on the FUSE handlers
	// still protect callers that opt into a non-zero cache, but the
	// configured window does leave a race where reload-after-stat can
	// serve a previously-cached "exists" answer. Documented in
	// MountOptions.CacheTimeout.
	cacheDur := opts.CacheTimeout
	if cacheDur < 0 {
		cacheDur = 0
	}
	mountOpts := &fs.Options{
		MountOptions: fuse.MountOptions{
			Debug:             opts.Debug,
			Name:              "fuse",
			FsName:            fsName,
			DirectMount:       opts.DirectMount,
			DirectMountStrict: opts.DirectMount,
		},
		EntryTimeout: &cacheDur,
		AttrTimeout:  &cacheDur,
	}
	srv, err := fs.Mount(mountPoint, root, mountOpts)
	if err != nil {
		return nil, err
	}
	return srv, nil
}
