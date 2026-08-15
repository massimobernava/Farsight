#!/usr/bin/env bash
# Fetches the latest (or a given) farsight-client .deb from a GitHub Release
# and installs it. Run as root. All of farsight-client's dependencies
# (x11vnc, python3-websockify, novnc) are in stock Ubuntu repos — no extra
# apt repo/key setup needed, unlike the server side.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/massimobernava/Farsight/main/packaging/client/install.sh | sudo bash
#   sudo bash install.sh v0.2.0          # install a specific release tag
#   sudo bash install.sh ./farsight-client_0.1.0_amd64.deb   # install a local .deb, skip download
#
# While the Farsight repo is private, GitHub's API needs auth to see
# releases/assets: export GITHUB_TOKEN (a PAT with repo read access) before
# running this script. Not needed once the repo is public.
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive
APT_YES=(-y -o Dpkg::Options::=--force-confold)

REPO="massimobernava/Farsight"
ARG="${1:-latest}"

if [ "$(id -u)" -ne 0 ]; then
    echo "install.sh: run as root (sudo)." >&2
    exit 1
fi

ARCH="$(dpkg --print-architecture)"
WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

echo "==> installing prerequisites (curl, jq)"
apt-get update -qq
apt-get install "${APT_YES[@]}" curl ca-certificates jq

if [[ "$ARG" == *.deb ]]; then
    DEB_PATH="$ARG"
    echo "==> installing local $DEB_PATH"
else
    VERSION="$ARG"
    if [ "$VERSION" = "latest" ]; then
        API_URL="https://api.github.com/repos/$REPO/releases/latest"
    else
        API_URL="https://api.github.com/repos/$REPO/releases/tags/$VERSION"
    fi

    AUTH_ARGS=()
    [ -n "${GITHUB_TOKEN:-}" ] && AUTH_ARGS=(-H "Authorization: Bearer $GITHUB_TOKEN")

    echo "==> looking up $VERSION release ($ARCH)"
    ASSET_JSON="$(curl -fsSL "${AUTH_ARGS[@]}" -H "Accept: application/vnd.github+json" "$API_URL")"
    ASSET_ID="$(echo "$ASSET_JSON" | jq -r --arg pat "farsight-client_.*_${ARCH}\\.deb" \
        '.assets[] | select(.name | test($pat)) | .id' | head -1)"
    ASSET_NAME="$(echo "$ASSET_JSON" | jq -r --arg pat "farsight-client_.*_${ARCH}\\.deb" \
        '.assets[] | select(.name | test($pat)) | .name' | head -1)"

    if [ -z "$ASSET_ID" ] || [ "$ASSET_ID" = "null" ]; then
        echo "install.sh: no farsight-client .deb found for $VERSION/$ARCH." >&2
        echo "  Check the release is published (not draft), and GITHUB_TOKEN is set if the repo is private." >&2
        exit 1
    fi

    DEB_PATH="$WORKDIR/$ASSET_NAME"
    echo "==> downloading $ASSET_NAME"
    curl -fsSL "${AUTH_ARGS[@]}" -H "Accept: application/octet-stream" \
        "https://api.github.com/repos/$REPO/releases/assets/$ASSET_ID" -o "$DEB_PATH"
fi

echo "==> installing farsight-client"
apt-get install "${APT_YES[@]}" "$DEB_PATH"

echo "==> done. Edit /etc/farsight/client.conf if needed, then:"
echo "    systemctl restart farsight-agent farsight-x11vnc farsight-vnc-proxy"
