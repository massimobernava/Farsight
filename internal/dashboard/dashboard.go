// Package dashboard renders the control plane's device list page — a
// minimal online/offline status per machine and a direct noVNC link,
// nothing more. Time-series telemetry (CPU/RAM/disk, custom metrics) is
// Grafana's job, not this page's.
package dashboard

import (
	"html/template"
	"io"

	"github.com/farsight/farsight/internal/registry"
)

// Config carries the bits needed to build per-device links.
type Config struct {
	WebsockifyPort int
	SSHUser        string // optional; empty means just show the IP
	// CanAccess reports whether the viewer should see a device's VNC/SSH
	// link at all — nil means "show it to everyone" (matches behavior
	// before this existed). This only hides the link in this page; the
	// real network access is still whatever the Tailscale ACL for that
	// device's tenant allows (see docs/MULTI-TENANCY.md Tappa 3b/4) — a
	// viewer with no link here isn't necessarily network-blocked, that's
	// noted future work, not implemented.
	CanAccess func(tenantID string) bool
	// IsAdmin hides the "Administration" link, and the per-device
	// tenant reassignment control, from anyone who'd just hit a 403 on
	// them — both are admin-only server-side (requireAdmin in
	// cmd/farsight-server).
	IsAdmin bool
	// AllTenants populates the reassignment dropdown — only rendered at
	// all when IsAdmin is set, harmless to leave empty otherwise.
	AllTenants []string
}

type deviceRow struct {
	registry.Device
	CanAccess bool
}

type pageData struct {
	Config
	Devices []deviceRow
}

var pageTemplate = template.Must(template.New("dashboard").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="refresh" content="15">
<link rel="icon" href="/favicon.ico">
<title>Farsight — Machines</title>
<style>
  body { font-family: system-ui, sans-serif; margin: 2rem; background: #111; color: #eee; }
  h1 { font-size: 1.2rem; }
  table { border-collapse: collapse; width: 100%; }
  th, td { text-align: left; padding: 0.4rem 0.8rem; border-bottom: 1px solid #333; }
  .online { color: #4caf50; }
  .offline { color: #888; }
  a { color: #64b5f6; }
  .muted { color: #888; font-size: 0.85em; }
  select { background: #222; color: #eee; border: 1px solid #444; padding: 0.15rem; }
</style>
</head>
<body>
<h1>Farsight — registered machines</h1>
{{if .IsAdmin}}<p class="muted"><a href="/tenants">Administration</a></p>{{end}}
<table>
<tr>
  <th>Name</th><th>Tenant</th><th>Device ID</th><th>Status</th><th>Tailscale IP</th>
  <th>Access</th>
</tr>
{{range .Devices}}
{{$dev := .}}
{{$tsIP := index .Attributes "tailscale_ip"}}
<tr>
  <td>
    <form method="post" action="/devices/{{.DeviceID}}/rename" style="display:flex;gap:0.3rem;">
      <input type="text" name="display_name" value="{{.DisplayName}}" placeholder="{{.DeviceID}}"
             style="width:9rem;background:#222;color:#eee;border:1px solid #444;padding:0.2rem;">
      <button type="submit" style="background:#333;color:#eee;border:1px solid #555;cursor:pointer;">save</button>
    </form>
  </td>
  <td>
    {{if $.IsAdmin}}
    <form method="post" action="/devices/{{$dev.DeviceID}}/reassign" style="display:flex;gap:0.2rem;">
      <select name="tenant_id">
        {{range $.AllTenants}}<option value="{{.}}" {{if eq . $dev.TenantID}}selected{{end}}>{{.}}</option>{{end}}
      </select>
      <button type="submit" style="background:#333;color:#eee;border:1px solid #555;cursor:pointer;">move</button>
    </form>
    {{else}}
      {{.TenantID}}
    {{end}}
  </td>
  <td>{{.DeviceID}}</td>
  <td>{{if .Online}}<span class="online">● online</span>{{else}}<span class="offline">● offline</span>{{end}}</td>
  <td>{{if $tsIP}}{{$tsIP}}{{else}}<span class="muted">n/a</span>{{end}}</td>
  <td>
    {{if not .CanAccess}}
      <span class="muted">access not allowed</span>
    {{else if and .Online $tsIP}}
      <a href="http://{{$tsIP}}:{{$.WebsockifyPort}}/vnc.html" target="_blank">Desktop</a>
      &middot; <span class="muted">ssh {{if $.SSHUser}}{{$.SSHUser}}@{{end}}{{$tsIP}}</span>
    {{else}}
      <span class="muted">unreachable</span>
    {{end}}
  </td>
</tr>
{{else}}
<tr><td colspan="6" class="muted">No machines registered yet.</td></tr>
{{end}}
</table>
</body>
</html>
`))

// Render writes the dashboard HTML page for the given device list.
func Render(w io.Writer, devices []registry.Device, cfg Config) error {
	rows := make([]deviceRow, len(devices))
	for i, d := range devices {
		can := true
		if cfg.CanAccess != nil {
			can = cfg.CanAccess(d.TenantID)
		}
		rows[i] = deviceRow{Device: d, CanAccess: can}
	}
	return pageTemplate.Execute(w, pageData{Config: cfg, Devices: rows})
}
