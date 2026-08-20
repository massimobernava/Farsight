<img src="assets/farsight.ico" width="64" height="64" alt="">

# Farsight

![build](https://github.com/massimobernava/Farsight/actions/workflows/build-deb.yml/badge.svg)
![license](https://img.shields.io/badge/license-Apache%202.0-blue)

Remote access (desktop + SSH) and monitoring for a fleet of Ubuntu machines, reachable only
from your Tailscale network — never exposed publicly.

Status: prototype, actively developed.

## Why Farsight

Most teams end up paying for tools like TeamViewer or AnyDesk to do something that, in a
private network you already control, doesn't need to leave that network at all: connect to a
machine's screen, run a command over SSH, or check whether a device is online and healthy.

Farsight does exactly that, self-hosted, over a [Tailscale](https://tailscale.com) (or
[Headscale](https://github.com/juanfont/headscale)) mesh you already run:

- **Remote desktop from the browser** — no client software to install on the operator's side,
  no relay servers, no third party ever sees the screen. Traffic goes device-to-device inside
  the VPN.
- **SSH** — plain SSH over the same private network, nothing extra to configure.
- **Fleet monitoring** — live status and historical metrics (Grafana) for every device, out of
  the box.
- **No public exposure, ever** — every service binds to the Tailscale interface only. There is
  nothing to firewall because there's nothing listening outside the VPN.

If you're currently paying per-seat for remote desktop software just to reach machines that are
already on a VPN you control, this is very likely a better fit — and free.

## Roadmap

Everything in this repo is open source and free to self-host, with no artificial limits on the
number of devices, and that's not going to change. The list below is what's already working
and what's planned next — checked off as it lands, not gated behind a paywall.

- [x] Browser-based remote desktop (VNC over Tailscale, no relay, no third party)
- [x] SSH access over the same private network
- [x] Fleet monitoring & dashboard (online/offline status, per-device naming)
- [x] Historical metrics (Grafana)
- [x] Multi-tenant (multiple isolated customers/teams on one server)
- [x] LLM assistant — natural-language queries over telemetry, anomaly triage (any
      OpenAI-compatible provider: local model via Ollama for fully on-prem setups, or hosted via
      OpenRouter for lower-cost/simpler setups)
- [ ] Fine-grained audit trail (who accessed which device, when)
- [ ] Headscale support as a drop-in alternative to Tailscale (self-hosted control plane)

Suggestions and PRs on any of the above are welcome.

## Managed hosting

Everything above is free to self-host yourself. If you'd rather not deal with running and
maintaining the infrastructure, that's the paid offering: I run a private control-plane server
for you (Headscale-based — one subscription instead of per-seat Tailscale pricing) and, if you
want the LLM assistant, a set number of tokens included per month (with the option to buy more
if you go over).

You don't need to set up or pay for anything separately — the server is mine, configured and
maintained, you just use it.

Interested? Open an issue or reach out directly — see [Support](#support) below.

## Support

This is currently a one-person project, maintained in spare time. Issues and pull requests are
welcome, but there's no guarantee of response time or fix turnaround on the open version — use
it accordingly, and feel free to fork it.

If you need setup help, guaranteed response times, or custom work, that's what the Premium /
consulting offering above is for — happy to discuss scope and pricing.

## Requirements

- Both client and server machines must be part of the same Tailscale network.
- If a machine doesn't already have Tailscale set up, you'll need an
  [auth key](https://login.tailscale.com/admin/settings/keys) to join it during install.

## Installing the server

One dedicated machine, for monitoring (dashboard + Grafana). `install.sh` adds the InfluxData
and Grafana apt repos it needs, downloads the latest `.deb` from
[Releases](https://github.com/massimobernava/Farsight/releases) and installs it — nothing to
download by hand.

```bash
export TS_AUTHKEY=tskey-auth-...           # only if this machine isn't already on Tailscale
curl -fsSL https://raw.githubusercontent.com/massimobernava/Farsight/main/packaging/server/install.sh \
  | sudo -E bash
```

To install a specific version instead of the latest, or from a `.deb` you already downloaded:

```bash
sudo -E bash install.sh v0.2.0
sudo -E bash install.sh ./farsight-server_0.1.0_amd64.deb
```

## Installing the client

On every machine you want to monitor/control remotely.

```bash
export TS_AUTHKEY=tskey-auth-...           # only if this machine isn't already on Tailscale
curl -fsSL https://raw.githubusercontent.com/massimobernava/Farsight/main/packaging/client/install.sh \
  | sudo -E bash
```

Then edit `/etc/farsight/client.conf` and set:
- `MQTT_BROKER` — the server's Tailscale IP (e.g. `tcp://100.x.x.x:1883`)
- `DEVICE_ID` — the name identifying this machine in the dashboard (defaults to the hostname,
  fine to leave as-is if it's already meaningful). Which tenant a device belongs to is assigned
  later, server-side, by an admin — nothing to configure on the client for that.

Then restart:

```bash
systemctl restart farsight-agent farsight-x11vnc farsight-vnc-proxy
```

## Usage

- **Dashboard**: `http://<server-tailscale-ip>:8080/` — machine list, online/offline status,
  direct link to each machine's remote desktop. You can give each machine a name (e.g.
  "Operating Room 1") in the field next to its ID: it's saved and survives a restart.
- **Grafana**: `http://<server-tailscale-ip>:3000/` — historical metrics. No login page: it
  identifies you the same way as the dashboard, via your Tailscale identity.

Both reachable only from inside the Tailscale network.

## Quick command-line test

Data travels over MQTT: two commands (`mosquitto-clients`) are enough to make a machine show
up in the dashboard without installing anything — handy for scripting too:

```bash
mosquitto_pub -h <server-tailscale-ip> -t 'farsight/test/status' -r -q 1 -m 'online'
mosquitto_pub -h <server-tailscale-ip> -t 'farsight/test/telemetry' -q 1 -m \
  '{"ts":"2026-08-13T12:00:00Z","device_id":"test","metrics":{"cpu_percent":12.5,"mem_percent":40.2,"disk_percent":55.0}}'
```

To integrate Farsight into an existing program (custom metrics, records, file uploads):
[sdk/c/README.md](sdk/c/README.md).

## License

Apache License 2.0 — see [LICENSE](LICENSE). Free to use, self-host, modify, and fork.

## A note on how this was built

Most of this codebase was written together with [Claude Code](https://claude.com/claude-code)
(Anthropic), working interactively with me across design decisions, implementation, and testing
on real hardware — not a one-shot generation. I review, direct, and validate every change; bugs
are still bugs and they're on me to fix. Mentioned here for transparency, not as a disclaimer.
