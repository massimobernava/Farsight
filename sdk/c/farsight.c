#include "farsight.h"

#include <MQTTClient.h>
#include <curl/curl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

struct farsight_client {
    MQTTClient mqtt;
    char *device_id;
    char *server_host; /* parsed from broker_url, reused for HTTP uploads */
    char *status_topic;
    char *data_topic;
    char *attributes_topic;
    char *records_topic;
};

static char *build_topic(const char *device_id, const char *kind) {
    size_t n = strlen("farsight/") + strlen(device_id) + 1 + strlen(kind) + 1;
    char *topic = malloc(n);
    if (topic) snprintf(topic, n, "farsight/%s/%s", device_id, kind);
    return topic;
}

/* "tcp://100.x.x.x:1883" -> "100.x.x.x". The HTTP control plane always
 * lives on the same host as the MQTT broker in a real deployment (one
 * farsight-server package installs both), so this is what
 * farsight_upload_file uses — see its doc comment. */
static char *extract_host(const char *broker_url) {
    const char *start = strstr(broker_url, "://");
    start = start ? start + 3 : broker_url;
    const char *colon = strchr(start, ':');
    size_t len = colon ? (size_t)(colon - start) : strlen(start);
    char *host = malloc(len + 1);
    if (host) {
        memcpy(host, start, len);
        host[len] = '\0';
    }
    return host;
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

static int publish(farsight_client *c, const char *topic, const char *payload, int retained) {
    if (!c) return -1;
    MQTTClient_deliveryToken token;
    int rc = MQTTClient_publish(c->mqtt, topic, (int)strlen(payload), payload, 1, retained, &token);
    if (rc != MQTTCLIENT_SUCCESS) return rc;
    return MQTTClient_waitForCompletion(c->mqtt, token, 5000);
}

/* Internal only — see farsight_connect's doc comment for why this isn't
 * exposed for callers to invoke themselves. */
static int set_status(farsight_client *c, int online) {
    return publish(c, c ? c->status_topic : NULL, online ? "online" : "offline", 1);
}

farsight_client *farsight_connect_from_config(const char *config_path) {
    if (!config_path) config_path = getenv("FARSIGHT_CLIENT_CONF");
    if (!config_path) config_path = "/etc/farsight/client.conf";

    char broker[256], device_id[256];
    if (!config_get(config_path, "MQTT_BROKER", broker, sizeof(broker))) return NULL;

    if (!config_get(config_path, "DEVICE_ID", device_id, sizeof(device_id)) ||
        device_id[0] == '\0') {
        if (gethostname(device_id, sizeof(device_id)) != 0) return NULL;
    }

    return farsight_connect(broker, device_id);
}

farsight_client *farsight_connect(const char *broker_url,
                                   const char *device_id) {
    farsight_client *c = calloc(1, sizeof(*c));
    if (!c) return NULL;

    c->device_id = strdup(device_id);
    c->server_host = extract_host(broker_url);
    c->status_topic = build_topic(device_id, "status");
    c->data_topic = build_topic(device_id, "telemetry");
    c->attributes_topic = build_topic(device_id, "attributes");
    c->records_topic = build_topic(device_id, "records");
    if (!c->device_id || !c->server_host || !c->status_topic ||
        !c->data_topic || !c->attributes_topic || !c->records_topic) {
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

    set_status(c, 1);
    return c;
}

int farsight_publish_series(farsight_client *c, const char *name, double value) {
    if (!c || !name) return -1;

    char ts[32];
    time_t now = time(NULL);
    strftime(ts, sizeof(ts), "%Y-%m-%dT%H:%M:%SZ", gmtime(&now));

    /* name is trusted to be a valid JSON object key (letters/digits/
     * underscore, no quotes) — see farsight_publish_series's doc comment. */
    char payload[512];
    snprintf(payload, sizeof(payload),
        "{\"ts\":\"%s\",\"device_id\":\"%s\","
        "\"metrics\":{\"%s\":%g}}",
        ts, c->device_id, name, value);

    return publish(c, c->data_topic, payload, 0);
}

int farsight_set_attribute_string(farsight_client *c, const char *key, const char *value) {
    if (!c || !key || !value) return -1;

    char ts[32];
    time_t now = time(NULL);
    strftime(ts, sizeof(ts), "%Y-%m-%dT%H:%M:%SZ", gmtime(&now));

    /* key/value are trusted not to contain unescaped quotes — see
     * farsight_set_attribute_string's doc comment. */
    char payload[768];
    snprintf(payload, sizeof(payload),
        "{\"ts\":\"%s\",\"device_id\":\"%s\","
        "\"key\":\"%s\",\"value\":\"%s\"}",
        ts, c->device_id, key, value);

    return publish(c, c->attributes_topic, payload, 0);
}

int farsight_set_attribute_double(farsight_client *c, const char *key, double value) {
    if (!c || !key) return -1;
    char value_str[64];
    snprintf(value_str, sizeof(value_str), "%g", value);
    return farsight_set_attribute_string(c, key, value_str);
}

int farsight_publish_record(farsight_client *c, const char *record_id,
                             const farsight_field *fields, int field_count) {
    if (!c || !record_id || (field_count > 0 && !fields)) return -1;

    char ts[32];
    time_t now = time(NULL);
    strftime(ts, sizeof(ts), "%Y-%m-%dT%H:%M:%SZ", gmtime(&now));

    /* Build the payload incrementally: field_count is caller-controlled,
     * a fixed-size buffer would silently truncate a large record. */
    size_t cap = 512;
    for (int i = 0; i < field_count; i++) {
        cap += strlen(fields[i].key) + strlen(fields[i].value) + 8;
    }
    char *payload = malloc(cap);
    if (!payload) return -1;

    int n = snprintf(payload, cap,
        "{\"ts\":\"%s\",\"device_id\":\"%s\","
        "\"record_id\":\"%s\",\"data\":{",
        ts, c->device_id, record_id);

    for (int i = 0; i < field_count; i++) {
        n += snprintf(payload + n, cap - (size_t)n, "%s\"%s\":\"%s\"",
                      i > 0 ? "," : "", fields[i].key, fields[i].value);
    }
    snprintf(payload + n, cap - (size_t)n, "}}");

    int rc = publish(c, c->records_topic, payload, 0);
    free(payload);
    return rc;
}

/* fread-compatible: libcurl's default POST read callback expects a FILE*
 * in CURLOPT_READDATA and calls fread itself when no CURLOPT_READFUNCTION
 * is set, so no explicit callback is needed here. */
struct response_buf {
    char *data;
    size_t len;
    size_t cap; /* leaves room for the trailing '\0' */
};

static size_t capture_response(char *ptr, size_t size, size_t nmemb, void *userdata) {
    struct response_buf *buf = (struct response_buf *)userdata;
    size_t add = size * nmemb;
    if (buf->len + add >= buf->cap) {
        add = buf->cap - buf->len - 1; /* truncate rather than fail the upload */
    }
    memcpy(buf->data + buf->len, ptr, add);
    buf->len += add;
    buf->data[buf->len] = '\0';
    return size * nmemb; /* tell curl we "handled" the whole chunk either way */
}

/* Shared by farsight_upload_file and farsight_upload_template: POST the
 * contents of file_path to url, capturing the (short, text) response body
 * into response_out if given. Returns 0 on 2xx, -1 on transport failure,
 * else the HTTP status code. */
static int http_post_file(const char *url, const char *file_path,
                           char *response_out, size_t response_out_len) {
    FILE *f = fopen(file_path, "rb");
    if (!f) return -1;
    fseek(f, 0, SEEK_END);
    long size = ftell(f);
    fseek(f, 0, SEEK_SET);

    CURL *curl = curl_easy_init();
    if (!curl) {
        fclose(f);
        return -1;
    }

    char response[512];
    struct response_buf buf = { response, 0, sizeof(response) };

    curl_easy_setopt(curl, CURLOPT_URL, url);
    curl_easy_setopt(curl, CURLOPT_POST, 1L);
    curl_easy_setopt(curl, CURLOPT_READDATA, f);
    curl_easy_setopt(curl, CURLOPT_POSTFIELDSIZE, size);
    curl_easy_setopt(curl, CURLOPT_WRITEFUNCTION, capture_response);
    curl_easy_setopt(curl, CURLOPT_WRITEDATA, &buf);

    CURLcode res = curl_easy_perform(curl);
    int result = -1;
    if (res == CURLE_OK) {
        long status = 0;
        curl_easy_getinfo(curl, CURLINFO_RESPONSE_CODE, &status);
        result = (status >= 200 && status < 300) ? 0 : (int)status;
        if (result == 0 && response_out && response_out_len > 0) {
            /* server responds with a short text answer + trailing newline */
            char *nl = strpbrk(response, "\r\n");
            if (nl) *nl = '\0';
            snprintf(response_out, response_out_len, "%s", response);
        }
    }

    curl_easy_cleanup(curl);
    fclose(f);
    return result;
}

int farsight_upload_file(farsight_client *c, int http_port, const char *file_path,
                          char *saved_filename_out, size_t saved_filename_out_len) {
    if (!c || !file_path) return -1;
    if (http_port <= 0) http_port = 8080;
    if (saved_filename_out && saved_filename_out_len > 0) saved_filename_out[0] = '\0';

    const char *filename = strrchr(file_path, '/');
    filename = filename ? filename + 1 : file_path;

    char url[768];
    snprintf(url, sizeof(url), "http://%s:%d/devices/%s/upload?filename=%s",
             c->server_host, http_port, c->device_id, filename);

    return http_post_file(url, file_path, saved_filename_out, saved_filename_out_len);
}

int farsight_upload_template(farsight_client *c, int http_port,
                              const char *name, const char *file_path) {
    if (!c || !name || !file_path) return -1;
    if (http_port <= 0) http_port = 8080;

    char url[768];
    snprintf(url, sizeof(url), "http://%s:%d/templates/%s", c->server_host, http_port, name);

    return http_post_file(url, file_path, NULL, 0);
}

int farsight_set_screen_export(farsight_client *c, int enable) {
    if (!c) return -1;

    /* Same unit order as farsight-screen-export itself: start the proxy
     * after x11vnc is already up, stop the proxy before x11vnc goes down
     * (the proxy sits in front of it). Not reinventing the toggle, just
     * calling the same systemctl invocation the shell script does. */
    const char *cmd = enable
        ? "systemctl enable --now farsight-x11vnc.service farsight-vnc-proxy.service"
        : "systemctl disable --now farsight-vnc-proxy.service farsight-x11vnc.service";

    int rc = system(cmd);
    if (rc == -1) return -1;
    if (WIFEXITED(rc)) return WEXITSTATUS(rc);
    return rc;
}

void farsight_disconnect(farsight_client *c) {
    if (!c) return;
    if (c->mqtt) {
        set_status(c, 0);
        MQTTClient_disconnect(c->mqtt, 1000);
        MQTTClient_destroy(&c->mqtt);
    }
    free(c->device_id);
    free(c->server_host);
    free(c->status_topic);
    free(c->data_topic);
    free(c->attributes_topic);
    free(c->records_topic);
    free(c);
}
