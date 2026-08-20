#!/bin/bash
# Regenerates the Tailscale-IP-dependent bits of Mosquitto/Telegraf config,
# plus the "Farsight - Sistema" dashboard's iframe URL, at every boot
# (systemd oneshot, runs before those services start — see
# farsight-server-netconfig.service and the *.service.d/farsight.conf
# drop-ins). Same rationale as the client's x11vnc/websockify wrappers: the
# Tailscale IP can change, and Mosquitto/Telegraf are stock upstream
# packages we don't otherwise control the startup of. Grafana itself is
# loopback-only now (see postinst) and no longer needs this.
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

# --- Grafana itself: no longer bound to the Tailscale IP here (see
# postinst) — it's loopback-only now, fronted by farsight-grafana-proxy,
# which resolves its own Tailscale IP at every start the same way
# farsight-server does, so it needs no boot-time config regeneration here.

# --- Telegraf: MQTT -> InfluxDB bridge, time-series data only ---
# internal/telemetry.Payload has no fixed metric fields at all beyond
# identity (ts/device_id) — everything else is under "metrics", a fully
# open bag. Tenant is not part of the wire format at all (see
# internal/telemetry package doc) — a device's tenant is a farsight-server
# admin assignment, not something Telegraf/InfluxDB ever need to know.
# Point-in-time attributes (Tailscale IP, whether the desktop is
# reachable, ...) travel on a separate MQTT topic entirely and go to
# farsight-server's SQLite store, not here — see docs/DEVELOPMENT.md.
TOKEN=""
[ -f /etc/farsight/influx_token ] && TOKEN=$(cat /etc/farsight/influx_token)
mkdir -p /etc/telegraf/telegraf.d
cat > /etc/telegraf/telegraf.d/farsight.conf <<EOF
[[inputs.mqtt_consumer]]
  servers = ["tcp://127.0.0.1:1883"]
  topics = ["farsight/+/telemetry"]
  data_format = "json_v2"
  [[inputs.mqtt_consumer.json_v2]]
    measurement_name = "telemetry"
    timestamp_path = "ts"
    timestamp_format = "2006-01-02T15:04:05.999999999Z07:00"
    [[inputs.mqtt_consumer.json_v2.tag]]
      path = "device_id"
    [[inputs.mqtt_consumer.json_v2.tag]]
      # Optional: correlates a burst of telemetry with a specific
      # record/session (see internal/telemetry.Payload.RecordID) — a
      # device treats more than one patient over its life, so time series
      # need to be filterable per occurrence, not just per device.
      path = "record_id"
      optional = true
    [[inputs.mqtt_consumer.json_v2.object]]
      # Every key under "metrics" becomes its own InfluxDB field
      # automatically, no config change needed per new metric name.
      path = "metrics"
      optional = true
      disable_prepend_keys = true

[[outputs.influxdb_v2]]
  urls = ["http://127.0.0.1:8086"]
  token = "${TOKEN}"
  organization = "farsight"
  bucket = "telemetry"
EOF


# --- Grafana: render the "Farsight - Sistema" dashboard (the device list,
# embedding farsight-server's own dashboard page) with this machine's
# current Tailscale IP baked into the iframe URL. Same rationale as the
# Mosquitto/Telegraf blocks above: the IP can change, this file can't be a
# static conffile. Provisioned from /etc/grafana/dashboards (see
# dashboards-provisioning.yaml) and set as the org's home dashboard in
# grafana.ini by postinst — the point is that a fresh install shows this
# on first login, not an empty Grafana someone has to go build themselves.
if [ -f /usr/share/farsight/grafana/sistema.json.tmpl ]; then
    mkdir -p /etc/grafana/dashboards
    sed "s#__FARSIGHT_URL__#http://${TS_IP}:${HTTP_PORT:-8080}/#" \
        /usr/share/farsight/grafana/sistema.json.tmpl > /etc/grafana/dashboards/farsight-sistema.json
fi

exit 0
