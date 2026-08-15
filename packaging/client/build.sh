#!/usr/bin/env bash
# Builds farsight-client_<version>_<arch>.deb. Run this inside the
# docker/Dockerfile.client-test container (needs golang-go and dpkg-dev).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PKG_SRC="$REPO_ROOT/packaging/client"
VERSION="${VERSION:-0.1.0}"
ARCH="${ARCH:-$(dpkg --print-architecture)}"
BUILD_DIR="$REPO_ROOT/packaging/client/build"
ROOT="$BUILD_DIR/root"
DIST_DIR="$REPO_ROOT/dist"

rm -rf "$BUILD_DIR"
mkdir -p "$ROOT/DEBIAN" \
         "$ROOT/usr/bin" \
         "$ROOT/lib/systemd/system" \
         "$ROOT/etc/farsight"

echo "==> building farsight-agent ($ARCH)"
( cd "$REPO_ROOT" && \
  CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -o "$ROOT/usr/bin/farsight-agent" ./cmd/farsight-agent )

echo "==> assembling package tree"
install -m 0755 "$PKG_SRC/usr/bin/farsight-x11vnc-wrapper" "$ROOT/usr/bin/farsight-x11vnc-wrapper"
install -m 0755 "$PKG_SRC/usr/bin/farsight-vnc-proxy-wrapper" "$ROOT/usr/bin/farsight-vnc-proxy-wrapper"

install -m 0644 "$PKG_SRC/lib/systemd/system/farsight-agent.service" "$ROOT/lib/systemd/system/"
install -m 0644 "$PKG_SRC/lib/systemd/system/farsight-x11vnc.service" "$ROOT/lib/systemd/system/"
install -m 0644 "$PKG_SRC/lib/systemd/system/farsight-vnc-proxy.service" "$ROOT/lib/systemd/system/"

install -m 0644 "$PKG_SRC/etc/farsight/client.conf.example" "$ROOT/etc/farsight/client.conf"

install -m 0755 "$PKG_SRC/debian/postinst" "$ROOT/DEBIAN/postinst"
install -m 0755 "$PKG_SRC/debian/prerm" "$ROOT/DEBIAN/prerm"
install -m 0644 "$PKG_SRC/debian/conffiles" "$ROOT/DEBIAN/conffiles"

sed -e "s/__VERSION__/$VERSION/" -e "s/__ARCH__/$ARCH/" \
    "$PKG_SRC/debian/control" > "$ROOT/DEBIAN/control"

mkdir -p "$DIST_DIR"
DEB_PATH="$DIST_DIR/farsight-client_${VERSION}_${ARCH}.deb"

echo "==> building $DEB_PATH"
dpkg-deb --build --root-owner-group "$ROOT" "$DEB_PATH"

echo "==> done: $DEB_PATH"
