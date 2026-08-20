#!/usr/bin/env bash
# Build and run the farsight-server test container (Mosquitto, Telegraf,
# InfluxDB 2.x, Grafana, later farsight-server), joined to the same tailnet
# as the client test container so the two can talk over real Tailscale IPs.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

IMAGE=farsight-server-test:latest
NAME=farsight-server-test

docker build -f docker/Dockerfile.server-test -t "$IMAGE" .

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
