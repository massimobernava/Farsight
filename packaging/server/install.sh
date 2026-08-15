#!/usr/bin/env bash
# Fetches the latest (or a given) farsight-server .deb from a GitHub Release
# and installs it. Run as root.
#
# Repos this package Depends: on (Telegraf, InfluxDB2, Grafana) live in
# third-party apt repos that don't exist on a stock Ubuntu machine — this
# script adds them and runs `apt-get update` *before* installing the .deb,
# so apt resolves the Depends: in the same transaction that installs
# farsight-server, in one atomic step. This is deliberately not done from
# the package's own postinst: dpkg holds the frontend lock for the whole
# transaction that runs postinst, and a nested apt-get from inside it would
# deadlock against its own parent.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/massimobernava/Farsight/main/packaging/server/install.sh | sudo bash
#   sudo bash install.sh v0.2.0          # install a specific release tag
#   sudo bash install.sh ./farsight-server_0.1.0_amd64.deb   # install a local .deb, skip download
#
# While the Farsight repo is private, GitHub's API needs auth to see
# releases/assets: export GITHUB_TOKEN (a PAT with repo read access) before
# running this script. Not needed once the repo is public.
set -euo pipefail

# Never prompt: apt has no controlling terminal when this runs via
# `curl | sudo bash`, and a stray conffile prompt (e.g. influxdata-archive-
# keyring shipping its own copy of the repo file we write below) would hang
# forever instead of failing loudly. --force-confold keeps whatever's on
# disk on conflict, which is always fine here — we (re)write these files
# ourselves on every run anyway.
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

echo "==> installing prerequisites (curl, gnupg, jq)"
apt-get update -qq
apt-get install "${APT_YES[@]}" curl ca-certificates gnupg jq

echo "==> adding InfluxData apt repo (Telegraf, InfluxDB2)"
mkdir -p /etc/apt/keyrings
curl -fsSL https://repos.influxdata.com/influxdata-archive.key \
    | gpg --batch --yes --no-tty --dearmor -o /etc/apt/keyrings/influxdata-archive.gpg
echo "deb [signed-by=/etc/apt/keyrings/influxdata-archive.gpg] https://repos.influxdata.com/debian stable main" \
    > /etc/apt/sources.list.d/influxdata.list

echo "==> adding Grafana apt repo"
curl -fsSL https://apt.grafana.com/gpg.key \
    | gpg --batch --yes --no-tty --dearmor -o /etc/apt/keyrings/grafana.gpg
echo "deb [signed-by=/etc/apt/keyrings/grafana.gpg] https://apt.grafana.com stable main" \
    > /etc/apt/sources.list.d/grafana.list

apt-get update -qq

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
    ASSET_ID="$(echo "$ASSET_JSON" | jq -r --arg pat "farsight-server_.*_${ARCH}\\.deb" \
        '.assets[] | select(.name | test($pat)) | .id' | head -1)"
    ASSET_NAME="$(echo "$ASSET_JSON" | jq -r --arg pat "farsight-server_.*_${ARCH}\\.deb" \
        '.assets[] | select(.name | test($pat)) | .name' | head -1)"

    if [ -z "$ASSET_ID" ] || [ "$ASSET_ID" = "null" ]; then
        echo "install.sh: no farsight-server .deb found for $VERSION/$ARCH." >&2
        echo "  Check the release is published (not draft), and GITHUB_TOKEN is set if the repo is private." >&2
        exit 1
    fi

    DEB_PATH="$WORKDIR/$ASSET_NAME"
    echo "==> downloading $ASSET_NAME"
    curl -fsSL "${AUTH_ARGS[@]}" -H "Accept: application/octet-stream" \
        "https://api.github.com/repos/$REPO/releases/assets/$ASSET_ID" -o "$DEB_PATH"
fi

echo "==> installing farsight-server"
apt-get install "${APT_YES[@]}" "$DEB_PATH"

echo "==> done. Dashboard and Grafana are reachable on this machine's Tailscale IP."
