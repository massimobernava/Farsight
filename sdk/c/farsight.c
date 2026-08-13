#include "farsight.h"

#include <MQTTClient.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

struct farsight_client {
    MQTTClient mqtt;
    char *tenant_id;
    char *device_id;
    char *status_topic;
    char *telemetry_topic;
};

static char *build_topic(const char *tenant_id, const char *device_id, const char *kind) {
    size_t n = strlen("farsight/") + strlen(tenant_id) + 1 + strlen(device_id) + 1 + strlen(kind) + 1;
    char *topic = malloc(n);
    if (topic) snprintf(topic, n, "farsight/%s/%s/%s", tenant_id, device_id, kind);
    return topic;
}

/* Minimal KEY=VALUE reader matching internal/config.ParseFile's format:
 * blank lines and lines starting with '#' are skipped, values may be
 * quoted. Returns 1 and fills *out if key is found, 0 otherwise. */
static int config_get(const char *path, const char *key, char *out, size_t out_len) {
    FILE *f = fopen(path, "r");
    if (!f) return 0;

    char line[512];
    int found = 0;
    size_t key_len = strlen(key);
    while (fgets(line, sizeof(line), f)) {
        char *p = line;
        while (*p == ' ' || *p == '\t') p++;
        if (*p == '#' || *p == '\n' || *p == '\0') continue;
        if (strncmp(p, key, key_len) != 0 || p[key_len] != '=') continue;

        char *value = p + key_len + 1;
        char *nl = strpbrk(value, "\r\n");
        if (nl) *nl = '\0';
        if (*value == '"' || *value == '\'') {
            char quote = *value;
            value++;
            char *end = strrchr(value, quote);
            if (end) *end = '\0';
        }
        snprintf(out, out_len, "%s", value);
        found = 1;
    }
    fclose(f);
    return found;
}

farsight_client *farsight_connect_from_config(const char *config_path) {
    if (!config_path) config_path = getenv("FARSIGHT_CLIENT_CONF");
    if (!config_path) config_path = "/etc/farsight/client.conf";

    char broker[256], tenant_id[128], device_id[256];
    if (!config_get(config_path, "MQTT_BROKER", broker, sizeof(broker))) return NULL;
    if (!config_get(config_path, "TENANT_ID", tenant_id, sizeof(tenant_id))) return NULL;

    if (!config_get(config_path, "DEVICE_ID", device_id, sizeof(device_id)) ||
        device_id[0] == '\0') {
        if (gethostname(device_id, sizeof(device_id)) != 0) return NULL;
    }

    return farsight_connect(broker, tenant_id, device_id);
}

farsight_client *farsight_connect(const char *broker_url,
                                   const char *tenant_id,
                                   const char *device_id) {
    farsight_client *c = calloc(1, sizeof(*c));
    if (!c) return NULL;

    c->tenant_id = strdup(tenant_id);
    c->device_id = strdup(device_id);
    c->status_topic = build_topic(tenant_id, device_id, "status");
    c->telemetry_topic = build_topic(tenant_id, device_id, "telemetry");
    if (!c->tenant_id || !c->device_id || !c->status_topic || !c->telemetry_topic) {
        farsight_disconnect(c);
        return NULL;
    }

    char client_id[128];
    snprintf(client_id, sizeof(client_id), "farsight-sdk-%s", device_id);

    if (MQTTClient_create(&c->mqtt, broker_url, client_id,
                           MQTTCLIENT_PERSISTENCE_NONE, NULL) != MQTTCLIENT_SUCCESS) {
        farsight_disconnect(c);
        return NULL;
    }

    /* Same "die without a clean disconnect -> broker marks us offline"
     * guarantee as farsight-agent, via MQTT's Last Will. */
    MQTTClient_willOptions will = MQTTClient_willOptions_initializer;
    will.topicName = c->status_topic;
    will.message = "offline";
    will.retained = 1;
    will.qos = 1;

    MQTTClient_connectOptions conn_opts = MQTTClient_connectOptions_initializer;
    conn_opts.keepAliveInterval = 20;
    conn_opts.cleansession = 1;
    conn_opts.will = &will;

    if (MQTTClient_connect(c->mqtt, &conn_opts) != MQTTCLIENT_SUCCESS) {
        farsight_disconnect(c);
        return NULL;
    }

    farsight_set_status(c, 1);
    return c;
}

static int publish(farsight_client *c, const char *topic, const char *payload, int retained) {
    if (!c) return -1;
    MQTTClient_deliveryToken token;
    int rc = MQTTClient_publish(c->mqtt, topic, (int)strlen(payload), payload, 1, retained, &token);
    if (rc != MQTTCLIENT_SUCCESS) return rc;
    return MQTTClient_waitForCompletion(c->mqtt, token, 5000);
}

int farsight_set_status(farsight_client *c, int online) {
    return publish(c, c ? c->status_topic : NULL, online ? "online" : "offline", 1);
}

int farsight_publish_telemetry(farsight_client *c,
                                double cpu_percent,
                                double mem_percent,
                                double disk_percent) {
    if (!c) return -1;

    char ts[32];
    time_t now = time(NULL);
    strftime(ts, sizeof(ts), "%Y-%m-%dT%H:%M:%SZ", gmtime(&now));

    char payload[512];
    snprintf(payload, sizeof(payload),
        "{\"ts\":\"%s\",\"tenant_id\":\"%s\",\"device_id\":\"%s\","
        "\"cpu_percent\":%g,\"mem_percent\":%g,\"disk_percent\":%g}",
        ts, c->tenant_id, c->device_id, cpu_percent, mem_percent, disk_percent);

    return publish(c, c->telemetry_topic, payload, 0);
}

int farsight_publish_metric(farsight_client *c, const char *name, double value) {
    if (!c || !name) return -1;

    char ts[32];
    time_t now = time(NULL);
    strftime(ts, sizeof(ts), "%Y-%m-%dT%H:%M:%SZ", gmtime(&now));

    /* name is trusted to be a valid JSON object key (letters/digits/
     * underscore, no quotes) — see farsight_publish_metric's doc comment. */
    char payload[512];
    snprintf(payload, sizeof(payload),
        "{\"ts\":\"%s\",\"tenant_id\":\"%s\",\"device_id\":\"%s\","
        "\"metrics\":{\"%s\":%g}}",
        ts, c->tenant_id, c->device_id, name, value);

    return publish(c, c->telemetry_topic, payload, 0);
}

void farsight_disconnect(farsight_client *c) {
    if (!c) return;
    if (c->mqtt) {
        farsight_set_status(c, 0);
        MQTTClient_disconnect(c->mqtt, 1000);
        MQTTClient_destroy(&c->mqtt);
    }
    free(c->tenant_id);
    free(c->device_id);
    free(c->status_topic);
    free(c->telemetry_topic);
    free(c);
}
