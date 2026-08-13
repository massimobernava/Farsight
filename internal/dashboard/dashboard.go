// Package dashboard renders the control plane's device list page: the
// "vetrina minima" from PROJECT_SPEC.md step 3 — online/offline status per
// machine and a direct noVNC link, nothing more.
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
}

type pageData struct {
	Config
	Devices []registry.Device
}

var pageTemplate = template.Must(template.New("dashboard").Parse(`<!doctype html>
<html lang="it">
<head>
<meta charset="utf-8">
<meta http-equiv="refresh" content="15">
<title>Farsight — Macchine</title>
<style>
  body { font-family: system-ui, sans-serif; margin: 2rem; background: #111; color: #eee; }
  h1 { font-size: 1.2rem; }
  table { border-collapse: collapse; width: 100%; }
  th, td { text-align: left; padding: 0.4rem 0.8rem; border-bottom: 1px solid #333; }
  .online { color: #4caf50; }
  .offline { color: #888; }
  a { color: #64b5f6; }
  .muted { color: #888; font-size: 0.85em; }
</style>
</head>
<body>
<h1>Farsight — macchine registrate</h1>
<table>
<tr>
  <th>Tenant</th><th>Device</th><th>Stato</th><th>IP Tailscale</th>
  <th>CPU%</th><th>RAM%</th><th>Disk%</th><th>x11vnc</th><th>websockify</th>
  <th>Accesso</th>
</tr>
{{range .Devices}}
<tr>
  <td>{{.TenantID}}</td>
  <td>{{.DeviceID}}</td>
  <td>{{if .Online}}<span class="online">● online</span>{{else}}<span class="offline">● offline</span>{{end}}</td>
  <td>{{if .TailscaleIP}}{{.TailscaleIP}}{{else}}<span class="muted">n/d</span>{{end}}</td>
  <td>{{printf "%.0f" .CPUPercent}}</td>
  <td>{{printf "%.0f" .MemPercent}}</td>
  <td>{{printf "%.0f" .DiskPercent}}</td>
  <td>{{if .ServiceX11VNCUp}}up{{else}}down{{end}}</td>
  <td>{{if .ServiceWebsockifyUp}}up{{else}}down{{end}}</td>
  <td>
    {{if .TailscaleIP}}
      <a href="http://{{.TailscaleIP}}:{{$.WebsockifyPort}}/vnc.html" target="_blank">Desktop</a>
      &middot; <span class="muted">ssh {{if $.SSHUser}}{{$.SSHUser}}@{{end}}{{.TailscaleIP}}</span>
    {{else}}
      <span class="muted">-</span>
    {{end}}
  </td>
</tr>
{{else}}
<tr><td colspan="10" class="muted">Nessuna macchina ancora registrata.</td></tr>
{{end}}
</table>
</body>
</html>
`))

// Render writes the dashboard HTML page for the given device list.
func Render(w io.Writer, devices []registry.Device, cfg Config) error {
	return pageTemplate.Execute(w, pageData{Config: cfg, Devices: devices})
}
