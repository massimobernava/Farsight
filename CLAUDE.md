# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project status

This repository currently contains only `PROJECT_SPEC.md` (in Italian) — no code has been
written yet. There is no build system, package manager, test suite, or lint config to run.
When implementation begins, this file should be updated with actual build/test/lint commands.

Treat `PROJECT_SPEC.md` as the source of truth for requirements and design decisions; read it
in full before starting implementation work. The summary below is derived from it.

## What Farsight is

Farsight is a remote access (desktop + SSH) and telemetry platform for a fleet of Ubuntu
machines connected over a Tailscale VPN mesh. Initial use case: IoT/industrial-medical devices
running LabVIEW that need remote supervision/debugging. It's meant as a more stable,
self-controlled alternative to TeamViewer, eventually resellable as a multi-tenant subscription
service — but the current phase is a **prototype/proof of concept** running on existing real
infrastructure (some machines already have Tailscale installed and configured; installers must
never destroy existing config).

## Core architectural principle

**The central server never proxies desktop/SSH traffic.** It only:
1. Lists/dashboards which machines are online (via MQTT telemetry + Tailscale status)
2. Controls permissions/tenancy
3. Dynamically generates direct links to each target machine's VPN IP

VNC and SSH traffic flows peer-to-peer through the Tailscale (WireGuard) tunnel, directly
between the operator's browser and the target machine — never through the control server.

## Two deliverables: client and server .deb packages

### Client package (`farsight-client.deb`) — installed on every monitored Ubuntu machine

- **Tailscale**: if already installed/configured, the installer must detect it and leave it
  alone (no reinstall/reconfigure). If absent, install it with `--login-server` pointed at the
  control server URL (Tailscale's official service for now; must stay parameterized for a
  future Headscale migration).
- **x11vnc**: attaches to the existing X11 session (no dedicated session). Must bind **only**
  to the local Tailscale interface (`-listen <tailscale-ip>`), never `0.0.0.0`. The Tailscale IP
  must be resolved dynamically at service start (it can change).
- **websockify**: local WebSocket↔TCP proxy bridging noVNC (browser) to x11vnc. Also bound only
  to the Tailscale interface.
- **noVNC**: static JS/HTML VNC client served from the same host (via websockify's static file
  serving or a small embedded web server).
- **`farsight-agent`**: telemetry process publishing periodically to the central MQTT broker:
  online/heartbeat status, current Tailscale IP, basic system metrics (CPU/RAM/disk, extensible
  later to LabVIEW-process-specific metrics), and local service status (x11vnc/websockify
  up/down) — so the dashboard can distinguish "online in VPN" from "VNC unreachable".
- **Registration**: on first boot the client registers with the server's control-plane API
  (inside the VPN) using a provisioning token to associate with the correct tenant/group. Can be
  simplified in the prototype (e.g. static tenant ID in config), but design the interface with
  future automated provisioning in mind.
- **Installer requirements**: idempotent (safe to re-run, never breaks an existing/authenticated
  Tailscale setup); must check whether services (x11vnc, websockify, MQTT agent) are already
  running before touching them; all services managed via dedicated `systemd` units with
  auto-restart. No new SSH daemon — reuse Ubuntu's system SSH over the Tailscale IP (session
  audit/logging is a later-phase concern, out of scope now).

### Server package (`farsight-server.deb`) — installed on one dedicated machine in the same Tailscale network

- **Mosquitto**: central MQTT broker receiving telemetry from all clients.
- **InfluxDB** (or TimescaleDB — TBD at implementation time): time-series storage for telemetry.
- **Grafana**: historical telemetry dashboards.
- **`farsight-server`** (the control plane, still to be built — this is the actual core of the
  project): registered-machine list with online/offline state (cross-referencing MQTT data and
  optionally the Tailscale API); per-machine direct link to
  `http://<machine-tailscale-ip>:<websockify-port>/vnc.html` for desktop access, plus the IP for
  manual SSH; user/permission management (minimal in the prototype, but designed for future
  multi-tenancy). No billing/subscriptions in the prototype.
- **Installer requirements**: idempotent and non-destructive like the client installer; the
  control-plane dashboard must only be exposed on the Tailscale interface, never publicly.

## Naming conventions (keep consistent across code/config)

- Repository: `farsight`
- Packages: `farsight-client.deb`, `farsight-server.deb`
- systemd services: `farsight-agent` (telemetry), `farsight-vnc-proxy` (websockify + noVNC),
  `farsight-server` (application control plane)
- Config files: `/etc/farsight/client.conf`, `/etc/farsight/server.conf`

## Security constraints (non-negotiable in this design)

- The entire system lives exclusively inside the VPN mesh. No service (x11vnc, websockify,
  control-plane dashboard, Grafana, Mosquitto) may ever bind to a non-Tailscale interface.
- Access control is delegated to **Tailscale/Headscale native ACLs** (tenant/department-scoped
  reachability), not to server-issued tokens/temporary passwords — that approach was
  deliberately rejected for now (may be revisited later for regulatory/audit needs in the
  medical context).
- Consider an x11vnc password as a defense-in-depth layer even though VPN membership is the
  primary access control.

## Tailscale vs. Headscale

Prototype phase uses Tailscale as-is (free tier, up to 100 devices) with zero extra
infrastructure, including already-configured test machines. A future commercial phase may
migrate self-hosted to Headscale (same `tailscale` client binary, only the `--login-server`
endpoint changes). **Because of this, the control-server/login-server URL must be parameterized
from the start** in every installer/config — the migration must be a config change, not a code
change. Data traffic (VNC/SSH/MQTT) never passes through the control/coordination server in
either case.

## Suggested build order (from the spec's roadmap)

1. Manual setup on 2 test machines (client with LabVIEW, server) to validate the full flow by
   hand: Tailscale (already present) → x11vnc → websockify → noVNC reachable from a third
   machine's browser inside the VPN.
2. Telemetry: MQTT publisher on the client side, Mosquitto + InfluxDB + Grafana on the server
   side; validate data arrives and is visualizable.
3. Minimal control plane: web page listing known machines (can be hardcoded at this stage) with
   online/offline state and a direct noVNC link per machine.
4. Package everything as separate client/server `.deb`s, with idempotency/existing-install
   detection logic.
5. Only after the prototype is validated: multi-tenancy, automated provisioning, advanced ACLs,
   possible Headscale migration, billing.

## Explicitly out of scope for the current prototype

- Billing / subscription management
- Zero-touch automated provisioning
- Advanced session audit trail (beyond basic logging)
- Headscale migration (keep in mind for design, don't implement)
- Specific regulatory compliance (GDPR/MDR etc. — address when nearing real use on health data)
- Local LLM assistant (Ollama + RTX 5090) — see "Phase 2" note below; don't implement now, but
  don't design against it either

## Phase 2 (future, not to implement now): local LLM assistant

Documented here only so today's architecture doesn't have to be reworked later. The central
server will eventually host **Ollama** with a local 30-35B model (e.g. Qwen2.5-32B) on an
**RTX 5090**, for: (1) a natural-language assistant over telemetry (NL→InfluxDB queries,
automated summaries, textual anomaly detection), and (2) a diagnostic assistant that triages a
device's logs/metrics/history before an operator connects via VNC.

**Hard constraint that already applies today**: everything must stay on-prem, inside the VPN —
no data ever leaves to external cloud APIs (medical/compliance requirement). Keep this as a
guiding principle even while the LLM itself doesn't exist yet.

Concrete implications for current design decisions:
- Structure `farsight-server` (control plane) so a future `farsight-assistant` module/endpoint
  can be added without heavy refactoring — avoid a rigid monolith where bolting on a new service
  means rewriting existing code.
- Design the InfluxDB/telemetry schema (field and tag names) to be clear, consistent, and
  semantically descriptive/query-friendly from the start, so an LLM can query historical data
  correctly later without needing it renamed or reinterpreted.
