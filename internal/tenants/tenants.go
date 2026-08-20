// Package tenants renders the control plane's tenant admin page — create,
// delete, and manage membership (which Tailscale login belongs to a
// tenant with which role). Admin-only (see cmd/farsight-server's
// requireAdmin).
package tenants

import (
	"html/template"
	"io"

	"github.com/farsight/farsight/internal/store"
)

type tenantRow struct {
	store.Tenant
	Members []store.TenantMember
}

// UserRow is one row of the "Users" table — FarsightAdmin and
// GrafanaAdmin are independent flags (see cmd/farsight-server's
// /admins and /grafana-admins), not linked in the UI: granting one
// doesn't require or imply the other, even though granting Farsight
// admin does cascade into Grafana admin server-side as a convenience.
type UserRow struct {
	Login         string
	FarsightAdmin bool
	GrafanaAdmin  bool
}

type pageData struct {
	Tenants      []tenantRow
	Users        []UserRow
	CurrentLogin string
}

var pageTemplate = template.Must(template.New("tenants").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<link rel="icon" href="/favicon.ico">
<title>Farsight — Administration</title>
<style>
  body { font-family: system-ui, sans-serif; margin: 2rem; background: #111; color: #eee; }
  h1 { font-size: 1.2rem; }
  table { border-collapse: collapse; width: 100%; margin-bottom: 1.5rem; }
  th, td { text-align: left; padding: 0.4rem 0.8rem; border-bottom: 1px solid #333; vertical-align: top; }
  a { color: #64b5f6; }
  .muted { color: #888; font-size: 0.85em; }
  input, select { background: #222; color: #eee; border: 1px solid #444; padding: 0.3rem; }
  button { background: #333; color: #eee; border: 1px solid #555; padding: 0.3rem 0.6rem; cursor: pointer; }
  .members li { font-size: 0.9em; }
  .members { margin: 0 0 0.4rem 0; padding-left: 1.1rem; }
</style>
</head>
<body>
<p class="muted"><a href="/">&larr; Machines</a> &middot; <a href="/llm-settings">LLM settings</a></p>
<h1>Farsight — users</h1>
<p class="muted">Farsight Admin and Grafana Admin are independent — you can enable either, both,
or neither. Becoming Farsight admin still automatically enables Grafana Admin too (not the other
way around). You can't remove yourself from Farsight admins.</p>
<table>
<tr><th>Login</th><th>Farsight Admin</th><th>Grafana Admin</th></tr>
{{range .Users}}
<tr>
  <td>{{.Login}}{{if eq .Login $.CurrentLogin}} <span class="muted">(you)</span>{{end}}</td>
  <td>
    {{if .FarsightAdmin}}
      {{if ne .Login $.CurrentLogin}}
      <form method="post" action="/admins/{{.Login}}/remove"
            onsubmit="return confirm('Remove {{.Login}} from Farsight admins?');">
        <button type="submit">remove</button>
      </form>
      {{else}}<span class="muted">admin</span>{{end}}
    {{else}}
    <form method="post" action="/admins">
      <input type="hidden" name="login" value="{{.Login}}">
      <button type="submit">make admin</button>
    </form>
    {{end}}
  </td>
  <td>
    {{if .GrafanaAdmin}}
    <form method="post" action="/grafana-admins/{{.Login}}/remove"
          onsubmit="return confirm('Remove {{.Login}} from Grafana Admin?');">
      <button type="submit">remove</button>
    </form>
    {{else}}
    <form method="post" action="/grafana-admins">
      <input type="hidden" name="login" value="{{.Login}}">
      <button type="submit">make admin</button>
    </form>
    {{end}}
  </td>
</tr>
{{else}}
<tr><td colspan="3" class="muted">No users known yet.</td></tr>
{{end}}
</table>
<div style="display:flex;gap:2rem;margin-bottom:2rem;">
  <form method="post" action="/admins" style="display:flex;gap:0.3rem;">
    <input type="email" name="login" placeholder="user@example.com" required style="width:13rem;">
    <button type="submit">+ Farsight Admin</button>
  </form>
  <form method="post" action="/grafana-admins" style="display:flex;gap:0.3rem;">
    <input type="email" name="login" placeholder="user@example.com" required style="width:13rem;">
    <button type="submit">+ Grafana Admin</button>
  </form>
</div>

<h1>Farsight — tenants</h1>

<table>
<tr><th>Tenant ID</th><th>Name</th><th>Created</th><th>Members (Tailscale login)</th><th></th></tr>
{{range .Tenants}}
<tr>
  <td>{{.TenantID}}</td>
  <td>{{.DisplayName}}</td>
  <td class="muted">{{.CreatedAt.Format "2006-01-02 15:04"}}</td>
  <td>
    <ul class="members">
    {{$tenantID := .TenantID}}
    {{range .Members}}
    <li>{{.Login}} <span class="muted">({{.Role}})</span>
      <form method="post" action="/tenants/{{$tenantID}}/members/{{.Login}}/remove" style="display:inline;">
        <button type="submit" style="font-size:0.75em;padding:0.05rem 0.3rem;">remove</button>
      </form>
    </li>
    {{else}}<li class="muted">none</li>{{end}}
    </ul>
    <form method="post" action="/tenants/{{.TenantID}}/members" style="display:flex;gap:0.3rem;">
      <input type="email" name="login" placeholder="user@example.com" required style="width:11rem;">
      <select name="role">
        <option value="viewer">viewer</option>
        <option value="operator">operator</option>
      </select>
      <button type="submit">add</button>
    </form>
  </td>
  <td>
    {{if ne .TenantID "default"}}
    <form method="post" action="/tenants/{{.TenantID}}/delete"
          onsubmit="return confirm('Delete tenant {{.TenantID}}? Assigned machines will move back to default.');">
      <button type="submit">delete</button>
    </form>
    {{end}}
  </td>
</tr>
{{else}}
<tr><td colspan="5" class="muted">No tenants registered yet.</td></tr>
{{end}}
</table>

<h1>New tenant</h1>
<form method="post" action="/tenants" style="display:flex;gap:0.5rem;">
  <input type="text" name="tenant_id" placeholder="tenant_id (e.g. acme-clinic)" required>
  <input type="text" name="display_name" placeholder="Name (e.g. Acme Clinic)">
  <button type="submit">create</button>
</form>

</body>
</html>
`))

// Render writes the tenant admin page. members maps tenant_id to its
// member list (login + role). currentLogin is the viewer's own, used to
// hide their own Farsight-admin "remove" button (self-removal is also
// blocked server-side, see requireAdmin's caller in cmd/farsight-server).
func Render(w io.Writer, ts []store.Tenant, members map[string][]store.TenantMember, users []UserRow, currentLogin string) error {
	rows := make([]tenantRow, len(ts))
	for i, t := range ts {
		rows[i] = tenantRow{Tenant: t, Members: members[t.TenantID]}
	}
	return pageTemplate.Execute(w, pageData{Tenants: rows, Users: users, CurrentLogin: currentLogin})
}
