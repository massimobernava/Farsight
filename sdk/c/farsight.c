#include "farsight.h"

#include <MQTTClient.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

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
