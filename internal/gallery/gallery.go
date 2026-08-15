// Package gallery renders a generic "here are the records matching these
// filters" page — no idea what any filter field means (a treatment ID, a
// patient ID, anything a caller chose), no idea whether a record has an
// image attached or not. If a record's data has a "filename" key, it's
// shown as an image (linking back to farsight-server's own /files/
// endpoint); everything else in a record's data is just shown as text.
// Driven entirely by the caller (typically a Grafana dashboard variable
// in an iframe panel's URL) — nothing here is specific to any one use
// case, see docs/DEVELOPMENT.md.
//
// The HTML layout itself is NOT compiled into this binary: it's loaded by
// name from a template file on disk (server.conf: TEMPLATES_DIR,
// <name>.html.tmpl) and re-read on every request. Editing or adding a
// page's look is a file, not a farsight-server rebuild — and since that
// file can itself be pushed with the same generic upload mechanism
// (POST /templates/<name>), not even a file *on the server you have to
// go touch* — that's the whole point, presentation is configurable the
// same way what-to-show already is (via the URL's filters).
package gallery

import (
	"html/template"
	"io"
	"os"

	"github.com/farsight/farsight/internal/store"
)

type pageData struct {
	Filters map[string]string
	Records []store.RecordMeta
}

// DefaultTemplate is what ships as templates/default.html.tmpl
// (packaging/server/etc/farsight/templates/default.html.tmpl) — also the
// fallback LoadTemplate uses if the requested file is missing or fails to
// parse, so a bad edit degrades to the built-in look instead of taking
// the endpoint down.
const DefaultTemplate = `<!doctype html>
<html lang="it">
<head>
<meta charset="utf-8">
<link rel="icon" href="/favicon.ico">
<title>Farsight — Record</title>
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
<p class="muted">Nessun record trovato per questi filtri.</p>
{{end}}
</body>
</html>
`

var defaultTemplate = template.Must(template.New("gallery").Parse(DefaultTemplate))

// LoadTemplate reads and parses templatePath. On any problem (missing
// file, bad syntax) it returns the built-in default template ALONGSIDE a
// non-nil error describing why — callers should log that error but can
// otherwise ignore it, the returned template is always safe to render
// with. Pass "" to always get the built-in default with no error.
func LoadTemplate(templatePath string) (*template.Template, error) {
	if templatePath == "" {
		return defaultTemplate, nil
	}
	content, err := os.ReadFile(templatePath)
	if err != nil {
		return defaultTemplate, err
	}
	t, err := template.New("gallery").Parse(string(content))
	if err != nil {
		return defaultTemplate, err
	}
	return t, nil
}

// Render executes an already-loaded template (see LoadTemplate) for the
// given filter set and matching records.
func Render(w io.Writer, t *template.Template, filters map[string]string, records []store.RecordMeta) error {
	return t.Execute(w, pageData{Filters: filters, Records: records})
}
