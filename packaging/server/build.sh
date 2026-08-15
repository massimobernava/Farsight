#!/usr/bin/env bash
# Builds farsight-server_<version>_<arch>.deb. Run this inside the
# docker/Dockerfile.server-test container (needs golang-go and dpkg-dev).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PKG_SRC="$REPO_ROOT/packaging/server"
VERSION="${VERSION:-0.1.0}"
ARCH="${ARCH:-$(dpkg --print-architecture)}"
BUILD_DIR="$REPO_ROOT/packaging/server/build"
ROOT="$BUILD_DIR/root"
DIST_DIR="$REPO_ROOT/dist"

rm -rf "$BUILD_DIR"
mkdir -p "$ROOT/DEBIAN" \
         "$ROOT/usr/bin" \
         "$ROOT/usr/lib/farsight" \
         "$ROOT/lib/systemd/system/mosquitto.service.d" \
         "$ROOT/lib/systemd/system/telegraf.service.d" \
         "$ROOT/lib/systemd/system/grafana-server.service.d" \
         "$ROOT/etc/farsight"

echo "==> building farsight-server ($ARCH)"
( cd "$REPO_ROOT" && \
  CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -o "$ROOT/usr/bin/farsight-server" ./cmd/farsight-server )

echo "==> assembling package tree"
install -m 0755 "$PKG_SRC/usr/lib/farsight/server-netconfig.sh" "$ROOT/usr/lib/farsight/server-netconfig.sh"

install -m 0644 "$PKG_SRC/lib/systemd/system/farsight-server.service" "$ROOT/lib/systemd/system/"
install -m 0644 "$PKG_SRC/lib/systemd/system/farsight-server-netconfig.service" "$ROOT/lib/systemd/system/"
install -m 0644 "$PKG_SRC/lib/systemd/system/mosquitto.service.d/farsight.conf" "$ROOT/lib/systemd/system/mosquitto.service.d/"
install -m 0644 "$PKG_SRC/lib/systemd/system/telegraf.service.d/farsight.conf" "$ROOT/lib/systemd/system/telegraf.service.d/"
install -m 0644 "$PKG_SRC/lib/systemd/system/grafana-server.service.d/farsight.conf" "$ROOT/lib/systemd/system/grafana-server.service.d/"

install -m 0644 "$PKG_SRC/etc/farsight/server.conf.example" "$ROOT/etc/farsight/server.conf"
mkdir -p "$ROOT/etc/farsight/templates"
install -m 0644 "$PKG_SRC/etc/farsight/templates/default.html.tmpl" "$ROOT/etc/farsight/templates/default.html.tmpl"

install -m 0755 "$PKG_SRC/debian/postinst" "$ROOT/DEBIAN/postinst"
install -m 0755 "$PKG_SRC/debian/prerm" "$ROOT/DEBIAN/prerm"
install -m 0644 "$PKG_SRC/debian/conffiles" "$ROOT/DEBIAN/conffiles"

sed -e "s/__VERSION__/$VERSION/" -e "s/__ARCH__/$ARCH/" \
    "$PKG_SRC/debian/control" > "$ROOT/DEBIAN/control"

mkdir -p "$DIST_DIR"
DEB_PATH="$DIST_DIR/farsight-server_${VERSION}_${ARCH}.deb"

echo "==> building $DEB_PATH"
dpkg-deb --build --root-owner-group "$ROOT" "$DEB_PATH"

echo "==> done: $DEB_PATH"
