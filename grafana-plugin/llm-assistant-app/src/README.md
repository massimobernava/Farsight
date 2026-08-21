# Farsight LLM Assistant

Ask questions about your fleet's devices, telemetry, and records, in plain language — answered by
an LLM with real read access to your tenant's data (device status, historical metrics, records),
never data outside it.

## Overview

This plugin adds a **Chat** page to Grafana: pick a tenant (if you belong to more than one),
type a question, get an answer grounded in real data — the assistant calls into
`farsight-server`'s own tools (list devices, fetch telemetry summaries/series, search records,
read a device's files, generate a downloadable report) to answer, and cites what it used. Past
conversations are kept per-login (private, not shared across a tenant) in a sidebar, each
deletable.

Works with any OpenAI-compatible provider — a local Ollama model for a fully on-prem setup with
nothing leaving your network, or a hosted provider like OpenRouter when you'd rather not run one
yourself. That choice, along with the model string, API key, system prompt, and per-metric
descriptions (what each telemetry field means and what's normal, so the assistant doesn't have to
guess from the field name alone), is configured on farsight-server's own `/llm-settings` admin
page — not in this plugin.

## Requirements

- A running `farsight-server` (part of the [Farsight](https://github.com/massimobernava/Farsight)
  platform) reachable from the browser over Tailscale.
- An LLM provider configured on that server's `/llm-settings` page — the chat page shows a clear
  message instead of a chat box if none is configured yet.

## Getting started

1. Install and enable this plugin (done automatically by `farsight-server`'s own package install
   — see the main project's README).
2. Open the **Configuration** page (admin only) and set the Farsight server's Tailscale URL.
3. Open **Chat**, pick a tenant, ask something.

## Documentation

Full setup, architecture, and the rest of the Farsight platform:
[github.com/massimobernava/Farsight](https://github.com/massimobernava/Farsight).

## Contributing

Issues and pull requests are welcome on the main
[Farsight repository](https://github.com/massimobernava/Farsight/issues) — this plugin isn't
maintained as a separate project.
