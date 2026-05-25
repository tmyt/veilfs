//go:build darwin

package cli

import "golang.org/x/sys/unix"

// probeCaseByStatfs maps common macOS filesystem type names to their
// usual case-sensitivity defaults. It is consulted only when the write
// probe fails (read-only source) so that mounts over snapshots and
// shared trees still default to a safe answer.
//
// The volume-level "case-sensitive" option of APFS is not exposed via
// f_fstypename; we lean toward case-insensitive (the macOS default) so
// callers can override with --case-mode=off when they know better.
func probeCaseByStatfs(dir string) (bool, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return false, false
	}
	// Convert Fstypename element-by-element so we accept both
	// []byte (current x/sys releases) and []int8 (older / future
	// SDK changes) without further code edits.
	var nameBytes [16]byte
	for i := 0; i < len(st.Fstypename) && i < len(nameBytes); i++ {
		nameBytes[i] = byte(st.Fstypename[i])
	}
	n := 0
	for n < len(nameBytes) && nameBytes[n] != 0 {
		n++
	}
	name := string(nameBytes[:n])
	switch name {
	case "hfs", "exfat", "msdos", "smbfs", "ntfs", "afpfs":
		return true, true
	case "apfs":
		// APFS containers ship in two flavors: the macOS default
		// (case-insensitive) and the opt-in case-sensitive variant
		// (used on some development volumes). Statfs cannot
		// distinguish them, so we decline to be confident here and
		// let the caller fall back to its safe default
		// (insensitive). Users on case-sensitive APFS volumes can
		// override with --case-mode=off.
		return false, false
	case "ufs", "zfs", "btrfs":
		// On macOS these filesystems default to case-sensitive
		// (POSIX-compatible) behaviour. The opt-in case-insensitive
		// variants of ZFS are rare; users on them can override with
		// --case-mode=on.
		return false, true
	}
	return false, false
}

