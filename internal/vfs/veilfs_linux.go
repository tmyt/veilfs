//go:build linux

package vfs

import (
	"context"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Statx is the Linux-only counterpart to Getattr that some callers
// (notably glibc 2.28+) use to read inode attributes including the
// btime/version fields. LoopbackNode exposes it on Linux but not macOS,
// so the guard lives in a Linux-only file. Without this method a path
// that became hidden after a hot reload could still answer statx(2)
// successfully via the cached dentry, leaking size/mode/mtime.
var _ fs.NodeStatxer = (*veilNode)(nil)

func (n *veilNode) Statx(ctx context.Context, f fs.FileHandle, flags uint32, mask uint32, out *fuse.StatxOut) syscall.Errno {
	if errno := n.guardSelf(); errno != 0 {
		return errno
	}
	return n.LoopbackNode.Statx(ctx, f, flags, mask, out)
}
