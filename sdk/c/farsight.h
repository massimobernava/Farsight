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

/* Publishes this machine's online/offline status (retained on the broker,
 * so it survives until the next update). Returns 0 on success. */
int farsight_set_status(farsight_client *client, int online);

/* Publishes one telemetry sample: CPU/RAM/disk usage as percentages
 * (0-100). Returns 0 on success. */
int farsight_publish_telemetry(farsight_client *client,
                                double cpu_percent,
                                double mem_percent,
                                double disk_percent);

/* Publishes status=offline, disconnects, and frees the client. Safe to
 * call with NULL. */
void farsight_disconnect(farsight_client *client);

#ifdef __cplusplus
}
#endif

#endif /* FARSIGHT_H */
