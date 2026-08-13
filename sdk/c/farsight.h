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

/* Publishes status=offline, disconnects, and frees the client. Safe to
 * call with NULL. */
void farsight_disconnect(farsight_client *client);

#ifdef __cplusplus
}
#endif

#endif /* FARSIGHT_H */
