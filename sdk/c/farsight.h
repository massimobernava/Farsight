/* Farsight C SDK — publish telemetry/status to a Farsight server without
 * touching MQTT directly. Wraps Eclipse Paho MQTT C internally; link with
 * -lpaho-mqtt3c (see README in this directory).
 */
#ifndef FARSIGHT_H
#define FARSIGHT_H

#ifdef __cplusplus
extern "C" {
#endif

typedef struct farsight_client farsight_client;

/* Connects to the Farsight MQTT broker for a given machine identity.
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

/* Publishes this machine's online/offline status (retained on the broker,
 * so it survives until the next update). Returns 0 on success. */
int farsight_set_status(farsight_client *client, int online);

/* Publishes one telemetry sample: CPU/RAM/disk usage as percentages
 * (0-100). Returns 0 on success. */
int farsight_publish_telemetry(farsight_client *client,
                                double cpu_percent,
                                double mem_percent,
                                double disk_percent);

/* Publishes one custom, application-specific metric under the given name —
 * anything not covered by farsight_publish_telemetry: patient counts,
 * sensor readings, whatever this machine wants to report. Pick a clear,
 * stable name: it becomes the InfluxDB field name as-is, nothing here
 * validates or namespaces it. name must be a valid JSON object key
 * (letters/digits/underscore, no quotes) — it's not escaped. Returns 0 on
 * success. */
int farsight_publish_metric(farsight_client *client,
                             const char *name,
                             double value);

/* Publishes status=offline, disconnects, and frees the client. Safe to
 * call with NULL. */
void farsight_disconnect(farsight_client *client);

#ifdef __cplusplus
}
#endif

#endif /* FARSIGHT_H */
