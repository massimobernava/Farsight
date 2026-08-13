#!/bin/bash
# Regenerates the Tailscale-IP-dependent bits of Mosquitto/Grafana/Telegraf
# config at every boot (systemd oneshot, runs before those services start —
# see farsight-server-netconfig.service and the *.service.d/farsight.conf
# drop-ins). Same rationale as the client's x11vnc/websockify wrappers: the
# Tailscale IP can change, and these three are stock upstream packages we
# don't otherwise control the startup of.
set -euo pipefail

CONF=/etc/farsight/server.conf
[ -f "$CONF" ] && source "$CONF"

TS_IP=$(tailscale ip -4 2>/dev/null | head -n1)
if [ -z "$TS_IP" ]; then
    echo "farsight-server-netconfig: no Tailscale IP yet, leaving existing config untouched" >&2
    exit 1
fi

# --- Mosquitto: Tailscale listener for remote clients, plus a loopback
# listener for same-host consumers (farsight-server, Telegraf). 127.0.0.1
# is never reachable off-box, so this doesn't reopen the "Tailscale only"
# requirement — it just avoids same-host services chasing a Tailscale IP
# that isn't needed for a local connection.
mkdir -p /etc/mosquitto/conf.d
cat > /etc/mosquitto/conf.d/farsight.conf <<EOF
listener 1883 127.0.0.1
listener 1883 ${TS_IP}
allow_anonymous true
EOF

# --- Grafana: bind only on the Tailscale interface ---
if [ -f /etc/grafana/grafana.ini ]; then
    if grep -q '^http_addr' /etc/grafana/grafana.ini; then
        sed -i "s/^http_addr.*/http_addr = ${TS_IP}/" /etc/grafana/grafana.ini
    else
        sed -i "s/^;http_addr.*/http_addr = ${TS_IP}/" /etc/grafana/grafana.ini
    fi
fi

# --- Telegraf: MQTT -> InfluxDB bridge ---
# Field/tag names here must match internal/telemetry.Payload's json tags —
# see PROJECT_SPEC.md "Fase 2" on why that schema needs to stay stable.
TOKEN=""
[ -f /etc/farsight/influx_token ] && TOKEN=$(cat /etc/farsight/influx_token)
mkdir -p /etc/telegraf/telegraf.d
cat > /etc/telegraf/telegraf.d/farsight.conf <<EOF
[[inputs.mqtt_consumer]]
  servers = ["tcp://127.0.0.1:1883"]
  topics = ["farsight/+/+/telemetry"]
  data_format = "json_v2"
  [[inputs.mqtt_consumer.json_v2]]
    measurement_name = "telemetry"
    timestamp_path = "ts"
    timestamp_format = "2006-01-02T15:04:05.999999999Z07:00"
    [[inputs.mqtt_consumer.json_v2.tag]]
      path = "tenant_id"
    [[inputs.mqtt_consumer.json_v2.tag]]
      path = "device_id"
    [[inputs.mqtt_consumer.json_v2.field]]
      path = "cpu_percent"
      type = "float"
    [[inputs.mqtt_consumer.json_v2.field]]
      path = "mem_percent"
      type = "float"
    [[inputs.mqtt_consumer.json_v2.field]]
      path = "disk_percent"
      type = "float"
    [[inputs.mqtt_consumer.json_v2.field]]
      path = "tailscale_ip"
      type = "string"
    [[inputs.mqtt_consumer.json_v2.field]]
      path = "service_x11vnc_up"
      type = "bool"
    [[inputs.mqtt_consumer.json_v2.field]]
      path = "service_websockify_up"
      type = "bool"

[[outputs.influxdb_v2]]
  urls = ["http://127.0.0.1:8086"]
  token = "${TOKEN}"
  organization = "farsight"
  bucket = "telemetry"
EOF

exit 0
