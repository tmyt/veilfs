#!/usr/bin/env bash
# Convenience wrapper: build the demo image (if needed) and drop the user
# into a veiled shell inside it. By default mounts ./sample-project; pass
# any directory as the first arg to mount that instead.
set -euo pipefail

DEMO_DIR=$(cd "$(dirname "$0")" && pwd)
PROJECT_ROOT=$(cd "$DEMO_DIR/../.." && pwd)

TARGET=${1:-$DEMO_DIR/sample-project}
TARGET=$(cd "$TARGET" && pwd)

IMG=veilfs-demo:local

# Build context = the veilfs repo root so the multi-stage build can see
# go.mod + sources. -f uses an absolute path to the Dockerfile so the
# command works regardless of the caller's working directory.
docker build -f "$DEMO_DIR/Dockerfile" -t "$IMG" "$PROJECT_ROOT"

echo
echo "=========================================================="
echo "host source : $TARGET"
echo "veiled view : /work (inside container)"
echo
echo "Try inside the container:"
echo "   ls"
echo "   cat README.md          # safe file"
echo "   cat .env               # hidden -> No such file"
echo "   echo HIJACK > .env     # write protection -> Permission denied"
echo "   cat secrets/api-key.pem  # hidden directory"
echo
echo "From another host terminal, edit:"
echo "   $TARGET/.veilignore"
echo "and watch the listing change in this shell (hot reload)."
echo "=========================================================="
echo

exec docker run --rm -it \
    --device /dev/fuse \
    --cap-add SYS_ADMIN \
    --security-opt apparmor=unconfined \
    -v "$TARGET:/work-source" \
    "$IMG"
