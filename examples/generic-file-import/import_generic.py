#!/usr/bin/env python3
"""NOT an official Farsight feature.

Generic example of how a proprietary/internal data format (here: the log
of a device monitoring an industrial batch process — key:value header,
[CALIBRATION] block, TSV table) can be parsed and loaded into the existing
system with zero changes to the server: the header (job, calibration,
final scores) as ONE record — accumulates, doesn't overwrite, since a
device runs more than one batch in its life — and the table as a time
series tagged with the same record_id, so Grafana can filter both to a
single batch. Same MQTT channels any publisher uses (see
docs/DEVELOPMENT.md).

See BATCH-000042-000891-081925.batch in this folder for a synthetic
sample file (made-up data, no real measurements).

Called by farsight-server as an importer (see uploadHandler in
cmd/farsight-server/main.go): after an upload with a .batch extension, if
this script is installed at /etc/farsight/importers/batch (executable),
farsight-server runs it as:

    import_generic.py <file-path> <device_id>

Can also run standalone, for testing:

    python3 import_generic.py <file.batch> <device_id>

Dependency: the python3-paho-mqtt Ubuntu package.
"""
import re
import sys
import time
import json
import paho.mqtt.publish as mqtt_publish

# Header key that identifies the batch — becomes record_id.
RECORD_ID_KEY = "BATCH ID"

# Header keys -> record field name (snake_case).
HEADER_MAP = {
    "DEVICE S/N": "device_sn",
    "DEVICE SW": "device_sw",
    "JOB ID": "job_id",
    "SETPOINT": "setpoint",
    "TARGET RATE": "target_rate",
    "OFFSET X": "offset_x",
    "OFFSET Y": "offset_y",
    "CAL ANGLE": "cal_angle",
    "C0": "cal_c0",
    "C1": "cal_c1",
    "C2": "cal_c2",
    "C3": "cal_c3",
    "BATCH ID": "batch_id",
    "QUALITY SCORE": "quality_score",
    "YIELD SCORE": "yield_score",
}

# Table columns -> metric name (snake_case, InfluxDB field). STATUS_BITS
# (a raw byte string) isn't a scalar metric, not mapped.
COLUMN_MAP = {
    "STAGE": "stage",
    "VALVE_OPEN": "valve_open",
    "VALVE_CURRENT": "valve_current",
    "VALVE_VOLTAGE": "valve_voltage",
    "FLOW_SENSOR": "flow_sensor",
    "HEATER_ON": "heater_on",
    "HEATER_CURRENT": "heater_current",
    "POWER_DENSITY": "power_density",
    "PUMP_A": "pump_a",
    "PUMP_B": "pump_b",
    "TEMPERATURE": "temperature",
    "CONCENTRATION": "concentration",
    "EFFICIENCY": "efficiency",
}


def parse(path):
    with open(path, encoding="ascii", errors="replace") as f:
        lines = f.read().splitlines()

    header = {}
    columns = None
    rows = []

    for line in lines:
        if line.startswith("---end of file---"):
            break
        if line.strip() in ("", "-"):
            continue
        if line.startswith("["):  # [CALIBRATION] / [END-CALIBRATION] markers
            continue

        if "\t" in line and columns is None and line.startswith("TIME"):
            columns = [c.strip() for c in line.split("\t")]
            continue

        if columns is not None:
            values = [v.strip() for v in line.split("\t")]
            if len(values) != len(columns):
                continue  # malformed table row, skip instead of crashing
            rows.append(dict(zip(columns, values)))
            continue

        key, sep, value = line.partition(":")
        if sep:
            header[key.strip()] = value.strip()

    return header, rows


def leading_number(s):
    """'1 (base)' -> 1.0 — STAGE sometimes has a text suffix."""
    m = re.match(r"[-+]?[0-9.eE+-]+", s)
    return float(m.group()) if m else None


def import_file(path, broker, device_id):
    header, rows = parse(path)

    record_id = header.get(RECORD_ID_KEY)
    if not record_id:
        print(f"no '{RECORD_ID_KEY}' in the file, can't associate a batch", file=sys.stderr)
        sys.exit(1)

    # --- record: ONE message with the whole header — accumulates
    # (a different record_id each time, a batch doesn't overwrite the
    # previous one) ---
    data = {
        field_name: header[header_key]
        for header_key, field_name in HEADER_MAP.items()
        if header_key in header
    }
    mqtt_publish.single(
        f"farsight/{device_id}/records",
        payload=json.dumps({
            "ts": _now_iso(),
            "device_id": device_id,
            "record_id": record_id,
            "data": data,
        }),
        hostname=broker_host(broker), port=broker_port(broker),
    )
    print(f"published record {record_id} with {len(data)} fields")

    # --- time series: one telemetry message per row, tagged with the same
    # record_id so Grafana can filter to a single batch. Timestamp
    # reconstructed from TIME (s) anchored to "now" (the file doesn't
    # contain a real batch start time, only elapsed seconds) ---
    if not rows:
        print("no table rows found, only the record was uploaded")
        return

    times = [leading_number(r.get("TIME (s)", "")) for r in rows]
    times = [t for t in times if t is not None]
    if not times:
        print("unreadable TIME (s) column, skipping the time series")
        return
    t_base = time.time() - max(times)

    data_msgs = []
    for row in rows:
        t_offset = leading_number(row.get("TIME (s)", ""))
        if t_offset is None:
            continue
        metrics = {}
        for col_name, metric_name in COLUMN_MAP.items():
            if col_name not in row:
                continue
            v = leading_number(row[col_name])
            if v is not None:
                metrics[metric_name] = v
        ts = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(t_base + t_offset))
        data_msgs.append({
            "topic": f"farsight/{device_id}/telemetry",
            "payload": json.dumps({
                "ts": ts,
                "device_id": device_id,
                "record_id": record_id,
                "metrics": metrics,
            }),
        })

    mqtt_publish.multiple(data_msgs, hostname=broker_host(broker), port=broker_port(broker))
    print(f"published {len(data_msgs)} time-series samples (record_id={record_id})")


def _now_iso():
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def broker_host(broker_url):
    # "tcp://host:port" -> host
    return broker_url.split("://")[-1].split(":")[0]


def broker_port(broker_url):
    return int(broker_url.split(":")[-1])


def read_broker_from_server_conf():
    import os
    path = os.environ.get("FARSIGHT_SERVER_CONF", "/etc/farsight/server.conf")
    try:
        with open(path) as f:
            for line in f:
                if line.startswith("MQTT_BROKER="):
                    return line.split("=", 1)[1].strip()
    except FileNotFoundError:
        pass
    return "tcp://127.0.0.1:1883"


if __name__ == "__main__":
    if len(sys.argv) != 3:
        print(f"usage: {sys.argv[0]} <file.batch> <device_id>", file=sys.stderr)
        sys.exit(1)

    file_path, device_id = sys.argv[1], sys.argv[2]
    import_file(file_path, read_broker_from_server_conf(), device_id)
