# Example: importer for proprietary log files

**Not an official Farsight feature.** Demonstrates that the existing system (generic upload +
MQTT) can absorb a proprietary/internal data format — here, the log of a device monitoring an
industrial batch process (e.g. a dosing/chemical-reaction plant) — without the specific format
ever entering `farsight-server`'s own codebase. The same approach works for any other format: a
measurement instrument's `.dat` file, a PLC log, a SCADA export — only the script installed as
the importer changes, never `farsight-server` itself.

The sample file (`BATCH-000042-000891-081925.batch`) is **synthetic** — generated data, no real
measurements — designed to have a realistic structure (fixed-field header, a calibration block,
a multi-column table) without containing anything sensitive: safe to keep in the public repo.

## How it works

1. The client sends the file to the server with the official SDK:
   `farsight_upload_file(client, 0, "batch.batch")` — see [`sdk/c/farsight.h`](../../sdk/c/farsight.h).
2. `farsight-server` saves the file, then looks for an executable script named after the file's
   extension inside `IMPORTERS_DIR` (default `/etc/farsight/importers/`, see `server.conf`) —
   for a `.batch` file, it looks for `/etc/farsight/importers/batch`. If it doesn't exist, the
   file is simply saved, no error: the core knows nothing about the format.
3. If the script exists, the server runs it as:
   `<script> <file-path> <device_id>` — [`import_generic.py`](import_generic.py) does exactly
   that: parses the format (key:value header, a `[CALIBRATION]` block, a TSV table), and
   publishes over MQTT using **the same channels any publisher uses**:
   - the header (job, calibration parameters, final scores) → **one record** (topic `records`,
     `record_id` = the file's `BATCH ID`) → SQLite, **accumulates** — a device runs more than
     one batch in its life, an attribute would overwrite the previous batch, a record doesn't
   - the table (`TIME (s)` column as the axis) → topic `telemetry`, **tagged with the same
     `record_id`** → InfluxDB, queryable/plottable history in Grafana filtered to a single batch

None of this specific format's logic lives in `cmd/farsight-server` or `internal/`.

## Seeing it in Grafana

Same pattern as any `record_id`-tagged record/telemetry: a dashboard with a dropdown populated
from known `record_id`/batch values (SQLite query `SELECT record_id AS __value, ... FROM
device_records`), a detail panel (read from SQLite via `json_extract`), and InfluxDB charts
filtered to the selected batch.

## Trying it

```bash
# 1. install the script as the importer for the .batch extension
sudo mkdir -p /etc/farsight/importers
sudo cp import_generic.py /etc/farsight/importers/batch
sudo chmod +x /etc/farsight/importers/batch
sudo apt-get install python3-paho-mqtt

# 2. send the file with the official SDK (farsight_upload_file, see sdk/c/README.md)
#    or, for a quick test without writing any C, straight from the CLI:

curl -X POST --data-binary @BATCH-000042-000891-081925.batch \
  "http://<server-tailscale-ip>:8080/devices/9042001/upload?filename=batch.batch"
```

Device `9042001` (the file's `DEVICE S/N`) will show up in the dashboard/Grafana; batch
`BATCH-000042-000891` will show up as its associated record.

The sample file also has a small irregularity baked into the time series — deliberately not
described here: a good test of whether the LLM assistant finds it when asked to analyze the
batch, something a quick glance at the dashboard alone would likely miss. See
[`llm_metric_descriptions.json`](llm_metric_descriptions.json) for a reference set of
descriptions of what each of the 13 metrics actually means, its unit, and what's normal — add
them on the `/llm-settings` admin page (one metric at a time) so the assistant doesn't have to
guess from the bare field name.

## Known limitations (it's a demo, not a product)

- `TIME (s)` in the file is seconds elapsed since the batch started, not a real timestamp — the
  reconstructed timestamp anchors the last row to "now" and counts backward. A real integration
  would need the actual batch start time, which isn't present in this format.
- The `STATUS_BITS` column (a byte string) isn't mapped — it isn't a scalar metric.
- `record_id` (from the file's `BATCH ID`) is the key linking the record and its time series — if
  two different devices happened to produce the same `BATCH ID`, they'd mix together under
  Grafana's filter. Not validated/made unique beyond trusting the device's own data.
