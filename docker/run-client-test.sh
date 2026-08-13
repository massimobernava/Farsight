#!/usr/bin/env bash
# Build and run the farsight-client test container: a systemd-enabled Ubuntu
# 24.04 box used to build the .deb and install/exercise it as if it were a
# real target machine (real systemd units, real Xvfb display, real tailnet
# join if TS_AUTHKEY is set).
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

IMAGE=farsight-client-test:latest
NAME=farsight-client-test

docker build -f docker/Dockerfile.client-test -t "$IMAGE" .

docker rm -f "$NAME" >/dev/null 2>&1 || true

docker run -d \
    --name "$NAME" \
    --privileged \
    --cgroupns=host \
    --tmpfs /run \
    --tmpfs /run/lock \
    -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
    -v "$(pwd)":/workspace \
    -w /workspace \
    -e TS_AUTHKEY="${TS_AUTHKEY:-}" \
    "$IMAGE"

echo "Container '$NAME' avviato. Shell:"
echo "  docker exec -it $NAME bash"
