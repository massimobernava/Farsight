# Farsight C SDK

Publish data to a Farsight server without handling MQTT or HTTP directly. Three data kinds, an
explicit choice by the caller — none is "standard" or mandatory:

- `farsight_publish_series(client, name, double value)` — **time series**, accumulates in
  InfluxDB over time (CPU, a sensor, a repeated reading).
- `farsight_set_attribute_string` / `farsight_set_attribute_double(client, key, value)` —
  **point-in-time value**, overwrites instead of accumulating (a Tailscale IP, a firmware
  version). Goes to SQLite, not InfluxDB.
- `farsight_publish_record(client, record_id, fields, field_count)` — **occurrence**, accumulates
  like a time series but is a whole object instead of a single value (a treatment, a visit, an
  uploaded file). A device that serves more than one patient in its life uses this, not
  attributes — an attribute would overwrite the previous patient.

Plus `farsight_upload_file(client, http_port, path, saved_filename_out, out_len)` — uploads a
whole file to the server (backups, images, data too large for a series/record). The server
renames the file to guarantee zero collisions (same device, back-to-back uploads with the same
name — e.g. several images for one treatment, one upload after another) and returns the real
name in `saved_filename_out`: use it to link the file to something else with
`farsight_publish_record` (a treatment, etc. — `farsight-server` has no way of knowing on its own
"this file belongs to that record," you build the link yourself with a field in the record, e.g.
`treatment_id`). Plus `farsight_upload_template(client, http_port, name, path)` — same idea but
for the *layout* `GET /records` uses to display data: overwrites by name instead of accumulating
(a template is a resource you update, not an event you accumulate). And
`farsight_connect_from_config` / `farsight_connect`, `farsight_disconnect`.

See [`farsight.h`](farsight.h) for every function's details and [`example.c`](example.c) for a
complete usage example. For a real end-to-end example (uploading a file, a server-side importer
that parses it, data ending up in Grafana with a per-batch dropdown):
[`examples/generic-file-import/`](../../examples/generic-file-import/).

## Dependencies

```bash
# Ubuntu/Debian
sudo apt-get install libpaho-mqtt-dev libcurl4-openssl-dev
```

## Build

```bash
cc example.c farsight.c -o farsight-example -lpaho-mqtt3c -lcurl
./farsight-example tcp://<server-tailscale-ip>:1883 my-device
```

To integrate into an existing program: compile `farsight.c` together with your own sources,
`#include "farsight.h"`, link `-lpaho-mqtt3c -lcurl`.

## Example

```c
farsight_client *c = farsight_connect_from_config(NULL); /* reads client.conf */

farsight_publish_series(c, "cpu_percent", 12.5);               /* -> InfluxDB, history */
farsight_set_attribute_string(c, "firmware_version", "1.4.2"); /* -> SQLite, current state */

farsight_field fields[] = {
    {"patient_id", "PID-001"},
    {"outcome", "success"},
};
farsight_publish_record(c, "TREATMENT-001", fields, 2);      /* -> SQLite, accumulates */

/* File linked to a treatment: upload it, then create a record that references it. */
char saved_name[256];
if (farsight_upload_file(c, 0, "/path/to/topography.jpg", saved_name, sizeof(saved_name)) == 0) {
    farsight_field image_fields[] = {
        {"filename", saved_name},
        {"treatment_id", "TREATMENT-001"},
        {"kind", "topography"},
    };
    farsight_publish_record(c, saved_name, image_fields, 3);
}

farsight_disconnect(c);
```

Names/keys become field names directly (InfluxDB) or JSON columns (SQLite) — any alphanumeric
string with underscores works; values/keys shouldn't contain unescaped quotes or backslashes (no
automatic escaping).
