// Package gallery renders a generic "here are the records matching this
// field/value" page — no idea what the field means (a treatment ID, a
// patient ID, anything a caller chose), no idea whether a record has an
// image attached or not. If a record's data has a "filename" key, it's
// shown as an image (linking back to farsight-server's own /files/
// endpoint); everything else in a record's data is just shown as text.
// Driven entirely by the caller (typically a Grafana dashboard variable
// in an iframe panel's URL) — nothing here is specific to any one use
// case, see docs/DEVELOPMENT.md.
package gallery

import (
	"html/template"
	"io"

	"github.com/farsight/farsight/internal/store"
)

type pageData struct {
	FieldName  string
	FieldValue string
	Records    []store.RecordMeta
}

var pageTemplate = template.Must(template.New("gallery").Parse(`<!doctype html>
<html lang="it">
<head>
<meta charset="utf-8">
<title>Farsight — Record: {{.FieldName}}={{.FieldValue}}</title>
<style>
  body { font-family: system-ui, sans-serif; margin: 1rem; background: #111; color: #eee; }
  .record { border: 1px solid #333; border-radius: 6px; padding: 0.8rem; margin-bottom: 1rem; }
  .record h3 { margin: 0 0 0.5rem 0; font-size: 0.95rem; color: #64b5f6; }
  .record img { max-width: 350px; max-height: 350px; display: block; margin-bottom: 0.5rem; border-radius: 4px; }
  table { border-collapse: collapse; }
  td { padding: 0.15rem 0.6rem 0.15rem 0; font-size: 0.85rem; }
  td.key { color: #888; }
  .muted { color: #888; }
</style>
</head>
<body>
{{range .Records}}
<div class="record">
  <h3>{{.RecordID}}</h3>
  {{if index .Data "filename"}}
    <a href="/devices/{{.TenantID}}/{{.DeviceID}}/files/{{index .Data "filename"}}" target="_blank">
      <img src="/devices/{{.TenantID}}/{{.DeviceID}}/files/{{index .Data "filename"}}" alt="{{.RecordID}}">
    </a>
  {{end}}
  <table>
  {{range $key, $value := .Data}}
    {{if ne $key "filename"}}<tr><td class="key">{{$key}}</td><td>{{$value}}</td></tr>{{end}}
  {{end}}
  </table>
</div>
{{else}}
<p class="muted">Nessun record con {{.FieldName}}={{.FieldValue}}.</p>
{{end}}
</body>
</html>
`))

// Render writes the gallery HTML page for the given field/value match.
func Render(w io.Writer, fieldName, fieldValue string, records []store.RecordMeta) error {
	return pageTemplate.Execute(w, pageData{FieldName: fieldName, FieldValue: fieldValue, Records: records})
}
