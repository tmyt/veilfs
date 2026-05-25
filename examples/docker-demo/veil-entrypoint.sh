#!/bin/sh
# Entrypoint that puts veilfs between a bind-mounted host directory and the
# command that the container ultimately runs.
#
# The host directory comes in at $VEILFS_SOURCE (read-write). Before
# starting veilfs we rebind that mount to a randomized internal path
# and unmount the well-known $VEILFS_SOURCE entry so user processes
# inside the container cannot trivially bypass the filter by reading
# the raw bind mount. This masking is best-effort: a root process
# could still discover the internal path via /proc/self/mountinfo.
# For production-grade isolation pair this with a non-root UID and
# 0700 ownership on the bind source, or with user namespaces.
set -e

SRC=${VEILFS_SOURCE:-/work-source}
DST=${VEILFS_TARGET:-/work}

if [ ! -d "$SRC" ]; then
    echo "veil-entrypoint: $SRC is not mounted." >&2
    echo "  Pass  -v \"\$(pwd):$SRC\"  on docker run." >&2
    exit 1
fi

mkdir -p "$DST"

INTERNAL=$(mktemp -d /tmp/.veilfs-XXXXXXXXXXXX)
mount --bind "$SRC" "$INTERNAL"
mount --make-private "$INTERNAL" 2>/dev/null || true
if ! umount "$SRC" 2>/dev/null; then
    # Bind mount could not be removed (older kernels / mount
    # propagation quirks). We do not chmod the source — that would
    # propagate through the bind mount to the host directory and
    # surprise users. Instead we warn and let the agent see
    # /work-source. Operators who need stronger isolation should run
    # the demo on a kernel that supports the unmount step or wire up
    # a dedicated user namespace.
    echo "veil-entrypoint: WARNING: could not umount $SRC; raw bind mount remains visible at $SRC" >&2
fi

veilfs mount "$INTERNAL" "$DST"

cd "$DST"
exec "$@"
