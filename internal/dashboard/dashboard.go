// Package dashboard renders the control plane's device list page: the
// "vetrina minima" from PROJECT_SPEC.md step 3 — online/offline status per
// machine and a direct noVNC link, nothing more. Time-series telemetry
// (CPU/RAM/disk, custom metrics) is Grafana's job, not this page's — see
// PROJECT_SPEC.md "Componente 2".
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
<link rel="icon" href="/favicon.ico">
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
  <th>Nome</th><th>Tenant</th><th>Device ID</th><th>Stato</th><th>IP Tailscale</th>
  <th>Accesso</th>
</tr>
{{range .Devices}}
{{$tsIP := index .Attributes "tailscale_ip"}}
<tr>
  <td>
    <form method="post" action="/devices/{{.TenantID}}/{{.DeviceID}}/rename" style="display:flex;gap:0.3rem;">
      <input type="text" name="display_name" value="{{.DisplayName}}" placeholder="{{.DeviceID}}"
             style="width:9rem;background:#222;color:#eee;border:1px solid #444;padding:0.2rem;">
      <button type="submit" style="background:#333;color:#eee;border:1px solid #555;cursor:pointer;">salva</button>
    </form>
  </td>
  <td>{{.TenantID}}</td>
  <td>{{.DeviceID}}</td>
  <td>{{if .Online}}<span class="online">● online</span>{{else}}<span class="offline">● offline</span>{{end}}</td>
  <td>{{if $tsIP}}{{$tsIP}}{{else}}<span class="muted">n/d</span>{{end}}</td>
  <td>
    {{if and .Online $tsIP}}
      <a href="http://{{$tsIP}}:{{$.WebsockifyPort}}/vnc.html" target="_blank">Desktop</a>
      &middot; <span class="muted">ssh {{if $.SSHUser}}{{$.SSHUser}}@{{end}}{{$tsIP}}</span>
    {{else}}
      <span class="muted">non raggiungibile</span>
    {{end}}
  </td>
</tr>
{{else}}
<tr><td colspan="6" class="muted">Nessuna macchina ancora registrata.</td></tr>
{{end}}
</table>
</body>
</html>
`))

// Render writes the dashboard HTML page for the given device list.
func Render(w io.Writer, devices []registry.Device, cfg Config) error {
	return pageTemplate.Execute(w, pageData{Config: cfg, Devices: devices})
}
