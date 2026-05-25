//go:build linux

package cli

import "golang.org/x/sys/unix"

// Filesystem magic numbers not always exported by older sys/unix
// versions. Sourced from <linux/magic.h> and Microsoft documentation.
const (
	ntfsSbMagic     = 0x5346544e // "NTFS" little-endian
	smb2MagicNumber = 0xFE534D42
	cifsMagicNumber = 0xFF534D42
)

// probeCaseByStatfs maps common Linux filesystem magic numbers to their
// usual case-sensitivity defaults. Used only when the write probe is
// unavailable (read-only source). The mapping leans on widely-deployed
// filesystems; obscure or virtual filesystems return (false, false) so
// the caller falls back to the conservative default.
func probeCaseByStatfs(dir string) (bool, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return false, false
	}
	switch int64(st.Type) {
	case unix.EXT4_SUPER_MAGIC,
		unix.XFS_SUPER_MAGIC,
		unix.BTRFS_SUPER_MAGIC,
		unix.TMPFS_MAGIC,
		unix.RAMFS_MAGIC,
		unix.OVERLAYFS_SUPER_MAGIC,
		unix.SQUASHFS_MAGIC:
		return false, true
	case unix.MSDOS_SUPER_MAGIC,
		unix.SMB_SUPER_MAGIC,
		ntfsSbMagic,
		smb2MagicNumber,
		cifsMagicNumber:
		return true, true
	}
	return false, false
}
