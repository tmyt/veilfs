package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// detectCaseInsensitive reports whether the directory at dir lives on a
// case-insensitive filesystem.
//
// The primary signal is a write probe: create a uniquely named temporary
// file and check whether an uppercase variant of the same name resolves
// to the same inode. This works regardless of filesystem type and
// respects volume-level options (e.g. APFS case-sensitive containers,
// ZFS casesensitivity).
//
// If the directory is read-only and the probe cannot be created,
// detectCaseInsensitive falls back to a heuristic based on filesystem
// type information from statfs/Statfs (see case_probe_*.go). When the
// fallback is also inconclusive, the function returns (true, nil) — the
// safer default for a secrecy tool, since classifying a case-sensitive
// FS as insensitive merely broadens matching, while classifying an
// insensitive FS as sensitive could let a renamed-case file slip past
// the matcher and reach the agent.
func detectCaseInsensitive(dir string) (bool, error) {
	v, ok, err := probeCaseByWrite(dir)
	if err == nil && ok {
		return v, nil
	}
	// Distinguish "expected" failures (the source is not writable) from
	// unexpected ones. Read-only filesystems return EROFS rather than
	// EACCES, so we accept both as a signal to fall back to the statfs
	// heuristic. Anything else (EIO, ENOSPC, ...) is propagated upward
	// so the caller can warn — silently degrading there would disguise
	// real I/O problems behind heuristic case behavior.
	if err != nil &&
		!errors.Is(err, fs.ErrPermission) &&
		!errors.Is(err, os.ErrPermission) &&
		!errors.Is(err, syscall.EROFS) {
		return false, err
	}
	if v, ok := probeCaseByStatfs(dir); ok {
		return v, nil
	}
	// Unable to determine; fail-closed (assume insensitive) so that
	// patterns continue to hide files whose case differs from the
	// .veilignore entry.
	return true, nil
}

// probeCaseByWrite drops a probe file into dir and stat's it with the
// case flipped. Returns (caseInsensitive, true, nil) on success;
// (false, false, err) when the probe could not be created.
func probeCaseByWrite(dir string) (bool, bool, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return false, false, err
	}
	// Append a fixed alphabetic suffix so ToUpper is guaranteed to
	// produce a different string; otherwise an all-digit hex suffix
	// would short-circuit the probe and miscount a case-insensitive
	// volume as case-sensitive about once in ten thousand mounts.
	base := ".veilfs-case-probe-" + hex.EncodeToString(buf[:]) + "z"
	probe := filepath.Join(dir, base)
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		return false, false, err
	}
	defer os.Remove(probe)

	upperName := strings.ToUpper(base)
	upper := filepath.Join(dir, upperName)
	if _, err := os.Stat(upper); err == nil {
		return true, true, nil
	} else if errors.Is(err, fs.ErrNotExist) {
		return false, true, nil
	} else {
		return false, false, err
	}
}
