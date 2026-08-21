# Farsight LLM Assistant (Grafana app plugin)

The chat panel behind Farsight's LLM assistant — natural-language questions about your fleet's
devices, telemetry, and records, answered by any OpenAI-compatible provider (local Ollama or
hosted OpenRouter, admin's choice — see the main [README](../../README.md)).

This is a **frontend-only** Grafana app plugin. There's no plugin backend: the chat panel talks
directly from the browser to `farsight-server`'s own `/llm/*` endpoints (CORS-enabled, see
`cmd/farsight-server/llmchat.go`) — all the actual chat/tool-calling logic lives there, not here.
This plugin is just the UI Grafana hosts it inside.

## What it does

- **Chat page** (`/a/farsight-llmassistant-app`) — pick a tenant, ask a question, get an answer
  with real telemetry/device data behind it. Sidebar shows past conversations (per-login,
  private), each deletable. New conversation, full-width growing input.
- **Configuration page** (admin-only) — one field: the Farsight server's Tailscale URL
  (`http://100.x.x.x:8080`). That's the only thing this plugin itself needs to know; the LLM
  provider, model, API key, and system prompt are all configured server-side on
  farsight-server's own `/llm-settings` admin page, not here.

## Not published to the Grafana plugin catalog

This isn't distributed via Grafana Cloud/the public catalog — `farsight-server`'s own `postinst`
installs it directly (`packaging/server/build.sh` builds the frontend, `postinst` copies it into
`/var/lib/grafana/plugins/` and allow-lists it as unsigned via
`allow_loading_unsigned_plugins` in `grafana.ini`). No Grafana Cloud account, no signing, no
`GRAFANA_API_KEY` needed for any of that — the signing/publishing sections `@grafana/create-plugin`
originally scaffolded into this README don't apply here and have been removed.

## Developing

```bash
npm install
npm run dev      # watch mode
npm run build     # production build (what packaging/server/build.sh runs)
npm run test:ci   # jest
npm run lint
```

To see it running against a real farsight-server: build a `.deb`
(`VERSION=x ARCH=amd64 bash packaging/server/build.sh` from the repo root) and install it, or use
the Docker test container (`docker/Dockerfile.server-test`) — see the main
[README](../../README.md) and `docs/DEVELOPMENT.md` (local, gitignored).

Backend plugin SDK/mage tooling that `@grafana/create-plugin` also scaffolds (`mage`, Go backend
binaries) isn't used — there's no backend here, only `npm`.
