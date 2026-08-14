/* Farsight C SDK — publish telemetry/status/files to a Farsight server
 * without touching MQTT or HTTP directly. Wraps Eclipse Paho MQTT C and
 * libcurl internally; link with -lpaho-mqtt3c -lcurl (see README in this
 * directory).
 */
#ifndef FARSIGHT_H
#define FARSIGHT_H

#include <stddef.h> /* size_t */

#ifdef __cplusplus
extern "C" {
#endif

typedef struct farsight_client farsight_client;

/* Connects to the Farsight MQTT broker for a given machine identity.
 * Online/offline status is automatic, not something you call yourself: it
 * publishes "online" the moment this succeeds, and sets an MQTT Last Will
 * so the broker publishes "offline" the instant the connection drops —
 * cleanly via farsight_disconnect, or on a crash/network loss. There's no
 * scenario where you'd have a connected client whose status should be
 * "offline," so nothing here lets you declare one out of sync with the
 * other.
 *
 * broker_url: e.g. "tcp://100.x.x.x:1883" (the server's Tailscale IP)
 * tenant_id, device_id: identify this machine in the dashboard/Grafana —
 *   keep them stable, they're also the InfluxDB tags used for history.
 *
 * Returns NULL on failure (connection refused, invalid broker_url, ...). */
farsight_client *farsight_connect(const char *broker_url,
                                   const char *tenant_id,
                                   const char *device_id);

/* Same as farsight_connect, but reads broker_url/tenant_id/device_id from a
 * farsight client.conf file (MQTT_BROKER, TENANT_ID, DEVICE_ID — same
 * format/keys as /etc/farsight/client.conf, since this is meant to run
 * alongside a real farsight-agent install and share its identity). Pass
 * NULL for config_path to use /etc/farsight/client.conf (or the
 * FARSIGHT_CLIENT_CONF environment variable, if set). DEVICE_ID defaults
 * to the machine's hostname when absent, same as farsight-agent.
 *
 * Returns NULL on failure (file missing/unreadable, MQTT_BROKER or
 * TENANT_ID missing from it, or the connection itself failing). */
farsight_client *farsight_connect_from_config(const char *config_path);

/* Publishes one TIME-SERIES sample under the given name — a value that
 * accumulates over time in InfluxDB (CPU load, a sensor reading, a patient
 * count taken repeatedly, anything you'd want to graph against time).
 * Nothing is "standard" here: CPU/RAM/disk are not privileged, call this
 * with whatever name/value makes sense — e.g.
 * farsight_publish_series(c, "cpu_percent", 12.5). Pick a clear, stable
 * name: it becomes the InfluxDB field name as-is, nothing here validates
 * or namespaces it. name must be a valid JSON object key (letters/digits/
 * underscore, no quotes) — it's not escaped. Returns 0 on success. */
int farsight_publish_series(farsight_client *client,
                             const char *name,
                             double value);

/* Publishes one POINT-IN-TIME attribute — a fact about current state that
 * OVERWRITES the previous value rather than accumulating (a Tailscale IP,
 * a firmware version, whether the desktop is reachable right now). Lands
 * in farsight-server's SQLite store, not InfluxDB — wrong tool for
 * anything you'd want a history of, use farsight_publish_series for that
 * instead. key must be a valid JSON object key (letters/digits/underscore)
 * and value must not contain unescaped double quotes or backslashes —
 * neither is JSON-escaped. Returns 0 on success. */
int farsight_set_attribute_string(farsight_client *client,
                                   const char *key,
                                   const char *value);

/* Same as farsight_set_attribute_string, for a numeric point value — does
 * the string conversion internally so a call site never has to (and can't
 * accidentally pass a bare 0 where a string was expected, silently
 * compiling as a null pointer — a real C footgun this avoids). Returns 0
 * on success. */
int farsight_set_attribute_double(farsight_client *client,
                                   const char *key,
                                   double value);

/* One key/value pair for farsight_publish_record. */
typedef struct {
    const char *key;
    const char *value;
} farsight_field;

/* Publishes a full snapshot tied to one OCCURRENCE of something — one
 * treatment session, one uploaded file, one visit — identified by
 * record_id. Unlike an attribute (single current value per key,
 * overwritten every time), records accumulate: a device that treats many
 * patients over its life gets one record per treatment, all kept, all
 * queryable later (Grafana: a record_id/patient dropdown backed by this
 * table — see docs/DEVELOPMENT.md). Publishing the same record_id again
 * replaces that one record (idempotent retry), not a new history entry.
 *
 * fields: array of field_count key/value pairs, the record's data. Same
 * escaping rule as farsight_set_attribute_string: no unescaped quotes or
 * backslashes in keys or values.
 *
 * Returns 0 on success. */
int farsight_publish_record(farsight_client *client,
                             const char *record_id,
                             const farsight_field *fields,
                             int field_count);

/* Uploads a whole file to the server (backups, bulk/structured data too
 * big or differently-shaped for farsight_publish_series/record — see
 * PROJECT_SPEC.md "Upload file client→server"). Talks to farsight-server's
 * HTTP control plane, not the MQTT broker — same host as broker_url
 * (that's always true in a real deployment: one farsight-server package
 * installs both), http_port is that host's HTTP_PORT from server.conf
 * (pass 0 for the default, 8080).
 *
 * farsight-server renames the file on save to guarantee it never collides
 * with another upload (same device, two files with the same name — e.g.
 * several images for one treatment, uploaded back to back) — the actual
 * saved name is written into saved_filename_out (pass NULL if you don't
 * need it). Typical use: upload an image, then farsight_publish_record
 * with that filename plus a field linking it back to whatever it belongs
 * to (a treatment's record_id, ...) — farsight-server has no built-in
 * concept of "this file belongs to that record," records are how you
 * build that link yourself. See examples/ophthalmic-tid-import/.
 *
 * farsight-server only stores the file; whether anything happens to it
 * next (parsing, pushing data back in via MQTT) depends on whether an
 * importer is configured for that extension.
 *
 * Returns 0 on success (HTTP 2xx), -1 on transport failure, or the HTTP
 * status code on a non-2xx response. */
int farsight_upload_file(farsight_client *client,
                          int http_port,
                          const char *file_path,
                          char *saved_filename_out,
                          size_t saved_filename_out_len);

/* Pushes the HTML template at file_path to the server as <name>.html.tmpl
 * (server.conf: TEMPLATES_DIR), for GET /records?...&template=<name> to
 * use. Unlike farsight_upload_file, this OVERWRITES any existing template
 * with the same name — a template is a named, deliberately mutable
 * resource ("update the gallery layout"), not an accumulating record of
 * uploads. This is how a page's *look* gets configured without ever
 * touching the server's filesystem directly: push a template from here,
 * point a Grafana panel's iframe at ?template=<name>, done — see
 * docs/DEVELOPMENT.md and examples/ophthalmic-tid-import/.
 *
 * Returns 0 on success (HTTP 2xx), -1 on transport failure, or the HTTP
 * status code on a non-2xx response. */
int farsight_upload_template(farsight_client *client,
                              int http_port,
                              const char *name,
                              const char *file_path);

/* Publishes status=offline, disconnects, and frees the client. Safe to
 * call with NULL. */
void farsight_disconnect(farsight_client *client);

#ifdef __cplusplus
}
#endif

#endif /* FARSIGHT_H */
