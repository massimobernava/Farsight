# Farsight

![build](https://github.com/massimobernava/Farsight/actions/workflows/build-deb.yml/badge.svg)

*[Versione italiana](README.md)*

Remote access (desktop + SSH) and monitoring for a fleet of Ubuntu machines, reachable only
from your Tailscale network — never exposed publicly.

Status: prototype.

## Requirements

- Both client and server machines must be part of the same Tailscale network.
- If a machine doesn't already have Tailscale set up, you'll need an
  [auth key](https://login.tailscale.com/admin/settings/keys) to join it during install.

## Installing the server

One dedicated machine, for monitoring (dashboard + Grafana).

```bash
export TS_AUTHKEY=tskey-auth-...   # only if this machine isn't already on Tailscale
dpkg -i farsight-server_*.deb
```

The latest `.deb` is on the [Releases](https://github.com/massimobernava/Farsight/releases) page.

## Installing the client

On every machine you want to monitor/control remotely.

```bash
export TS_AUTHKEY=tskey-auth-...   # only if this machine isn't already on Tailscale
dpkg -i farsight-client_*.deb
```

Then edit `/etc/farsight/client.conf` and set:
- `MQTT_BROKER` — the server's Tailscale IP (e.g. `tcp://100.x.x.x:1883`)
- `TENANT_ID` / `DEVICE_ID` — the name identifying this machine in the dashboard (`DEVICE_ID`
  defaults to the hostname; fine to leave as-is if it's already meaningful)

Then restart:

```bash
systemctl restart farsight-agent farsight-x11vnc farsight-vnc-proxy
```

## Usage

- **Dashboard**: `http://<server-tailscale-ip>:8080/` — machine list, online/offline status,
  direct link to each machine's remote desktop.
- **Grafana**: `http://<server-tailscale-ip>:3000/` (first login `admin`/`admin`, then a
  password change is required) — historical metrics.

Both reachable only from inside the Tailscale network.

## Documentation

- [PROJECT_SPEC.md](PROJECT_SPEC.md) — design and architectural decisions (Italian)
- [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) — build, test, release, for Farsight developers
