// Package llmsettings renders the control plane's LLM configuration page
// — the OpenAI-compatible provider connection (base URL, API key, model
// string), system prompt, and per-metric descriptions the assistant uses
// to interpret telemetry (see cmd/farsight-server/llmtools.go's
// metricInfo). Admin-only (see cmd/farsight-server's requireAdmin).
// Everything here is backed by internal/store's generic settings table —
// reading live, no restart needed for a change to take effect, same
// property tenant/admin management already has.
package llmsettings

import (
	"html/template"
	"io"
	"sort"
)

// MetricDescription is one row of the metric-descriptions table.
type MetricDescription struct {
	Name        string
	Description string
}

type pageData struct {
	ProviderURL         string
	Model               string
	SystemPrompt        string
	DefaultSystemPrompt string
	HasAPIKey           bool
	MetricDescriptions  []MetricDescription
}

var pageTemplate = template.Must(template.New("llmsettings").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<link rel="icon" href="/favicon.ico">
<title>Farsight — LLM Settings</title>
<style>
  body { font-family: system-ui, sans-serif; margin: 2rem; background: #111; color: #eee; max-width: 900px; }
  h1 { font-size: 1.2rem; }
  h2 { font-size: 1rem; margin-top: 2rem; }
  table { border-collapse: collapse; width: 100%; margin-bottom: 1rem; }
  th, td { text-align: left; padding: 0.4rem 0.8rem; border-bottom: 1px solid #333; vertical-align: top; }
  a { color: #64b5f6; }
  .muted { color: #888; font-size: 0.85em; }
  input, select, textarea { background: #222; color: #eee; border: 1px solid #444; padding: 0.4rem; font-family: inherit; }
  textarea { width: 100%; box-sizing: border-box; }
  button { background: #333; color: #eee; border: 1px solid #555; padding: 0.3rem 0.6rem; cursor: pointer; }
  label { display: block; margin-top: 1rem; margin-bottom: 0.3rem; }
  .field-desc { font-size: 0.85em; color: #888; margin: 0.2rem 0 0.4rem; }
</style>
</head>
<body>
<p class="muted"><a href="/">&larr; Machines</a> &middot; <a href="/tenants">Users &amp; tenants</a></p>
<h1>Farsight — LLM settings</h1>

<form method="post" action="/llm-settings">
  <label for="provider_url">Provider URL</label>
  <div class="field-desc">
    The OpenAI-compatible chat completions API base URL. Examples:
    <code>http://100.x.x.x:11434/v1</code> for Ollama on your tailnet,
    <code>https://openrouter.ai/api/v1</code> for OpenRouter.
  </div>
  <input type="text" name="provider_url" id="provider_url" value="{{.ProviderURL}}" placeholder="http://100.x.x.x:11434/v1" style="width:30rem;">

  <label for="provider_api_key">API key</label>
  <div class="field-desc">
    Leave empty for Ollama (no key needed). Required for OpenRouter and most hosted providers.
    {{if .HasAPIKey}}Currently set — leave blank to keep it unchanged.{{end}}
  </div>
  <input type="password" name="provider_api_key" id="provider_api_key" style="width:30rem;" placeholder="{{if .HasAPIKey}}(unchanged){{else}}sk-or-...{{end}}">

  <label for="model">Model</label>
  <div class="field-desc">
    The exact model identifier your provider expects — whatever you'd put in the "model" field of
    a raw API call. Examples: <code>qwen2.5:32b</code> (Ollama), <code>anthropic/claude-3.5-sonnet</code>
    or <code>meta-llama/llama-3.1-70b-instruct:free</code> (OpenRouter).
  </div>
  <input type="text" name="model" id="model" value="{{.Model}}" placeholder="qwen2.5:32b" style="width:30rem;">

  <label for="system_prompt">System prompt</label>
  <div class="field-desc">Leave empty to use the built-in default (shown below as placeholder).</div>
  <textarea name="system_prompt" id="system_prompt" rows="8" placeholder="{{.DefaultSystemPrompt}}">{{.SystemPrompt}}</textarea>

  <div style="margin-top:1rem;">
    <button type="submit">Save</button>
  </div>
</form>

<h2>Metric descriptions</h2>
<p class="muted">What each telemetry metric name means, its unit, and what a normal value looks
like — the assistant reads these to decide which metric is actually relevant to a question,
instead of guessing from the bare field name.</p>
<table>
<tr><th>Metric name</th><th>Description</th><th></th></tr>
{{range .MetricDescriptions}}
<tr>
  <td><code>{{.Name}}</code></td>
  <td>{{.Description}}</td>
  <td>
    <form method="post" action="/llm-settings/metrics/{{.Name}}/remove">
      <button type="submit">remove</button>
    </form>
  </td>
</tr>
{{else}}
<tr><td colspan="3" class="muted">No metric descriptions configured yet.</td></tr>
{{end}}
</table>
<form method="post" action="/llm-settings/metrics" style="display:flex;gap:0.5rem;align-items:flex-start;">
  <input type="text" name="name" placeholder="metric_name" required style="width:12rem;">
  <textarea name="description" placeholder="What it measures, its unit, what's normal..." required rows="2" style="flex:1;"></textarea>
  <button type="submit">Add</button>
</form>

</body>
</html>
`))

// Render writes the LLM settings page.
func Render(w io.Writer, providerURL, model, systemPrompt, defaultSystemPrompt string, hasAPIKey bool, descriptions map[string]string) error {
	names := make([]string, 0, len(descriptions))
	for name := range descriptions {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([]MetricDescription, len(names))
	for i, name := range names {
		rows[i] = MetricDescription{Name: name, Description: descriptions[name]}
	}

	return pageTemplate.Execute(w, pageData{
		ProviderURL: providerURL, Model: model,
		SystemPrompt: systemPrompt, DefaultSystemPrompt: defaultSystemPrompt,
		HasAPIKey: hasAPIKey, MetricDescriptions: rows,
	})
}
