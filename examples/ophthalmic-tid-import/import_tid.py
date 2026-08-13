#!/usr/bin/env python3
"""NON è una feature ufficiale di Farsight.

Esempio di come un formato dati proprietario/interno (i file .dat prodotti
da un dispositivo oftalmico — vedi data/TID-000001-000276-111125.dat nel
repo per un file reale) può essere interpretato e caricato nel sistema
esistente senza nessuna modifica al server: valori puntuali (paziente,
parametri intervento) come attributi, la tabella come serie temporale —
gli stessi due canali MQTT che usa qualunque publisher (vedi
docs/DEVELOPMENT.md).

Chiamato da farsight-server come importer (vedi uploadHandler in
cmd/farsight-server/main.go): dopo un upload con estensione .dat, se
questo script è installato in /etc/farsight/importers/dat (eseguibile),
farsight-server lo lancia con:

    import_tid.py <percorso-file> <tenant_id> <device_id>

Può anche girare da solo, per test:

    python3 import_tid.py <file.dat> <tenant_id> <device_id>

Dipendenza: pacchetto Ubuntu python3-paho-mqtt.
"""
import re
import sys
import time
import json
import paho.mqtt.publish as mqtt_publish

# Chiavi header -> nome attributo (snake_case, quello che finisce in SQLite).
HEADER_MAP = {
    "DEVICE S/N": "device_sn",
    "DEVICE SW": "device_sw",
    "PATIENT ID": "patient_id",
    "CCT": "cct",
    "KMax": "kmax",
    "Sph": "sph",
    "Cyl": "cyl",
    "Axis": "axis",
    "P0": "sensor_p0",
    "P1": "sensor_p1",
    "P2": "sensor_p2",
    "P3": "sensor_p3",
    "TREATMENT ID": "treatment_id",
    "RIBOSCORE": "riboscore",
    "THERASCORE": "therascore",
}

# Colonne tabella -> nome metrica (snake_case, campo InfluxDB). CBIT
# (stringa di byte grezza) non è una metrica scalare, non viene mappata.
COLUMN_MAP = {
    "PHASE": "phase",
    "IONTOPH": "iontoph",
    "IONTOCURR": "iontocurr",
    "IONTOVOLT": "iontovolt",
    "UV SENSOR": "uv_sensor",
    "UV-A LED": "uva_led",
    "UV-A CURR": "uva_curr",
    "POWER DNS": "power_dns",
    "BLUE LED": "blue_led",
    "GREEN LED": "green_led",
    "GREEN CH": "green_ch",
    "CONC": "conc",
    "EFFICACY": "efficacy",
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
        if line.startswith("["):  # [SENSOR] / [END-SENSOR] markers
            continue

        if "\t" in line and columns is None and line.startswith("TIME"):
            columns = [c.strip() for c in line.split("\t")]
            continue

        if columns is not None:
            values = [v.strip() for v in line.split("\t")]
            if len(values) != len(columns):
                continue  # riga tabella malformata, salta invece di crashare
            rows.append(dict(zip(columns, values)))
            continue

        key, sep, value = line.partition(":")
        if sep:
            header[key.strip()] = value.strip()

    return header, rows


def leading_number(s):
    """'1 (base)' -> 1.0 — PHASE a volte ha un suffisso testuale."""
    m = re.match(r"[-+]?[0-9.eE+-]+", s)
    return float(m.group()) if m else None


def import_file(path, broker, tenant_id, device_id):
    header, rows = parse(path)

    # --- attributi: un messaggio per chiave, stesso schema dell'SDK C ---
    attr_msgs = []
    for header_key, attr_name in HEADER_MAP.items():
        if header_key not in header:
            continue
        attr_msgs.append({
            "topic": f"farsight/{tenant_id}/{device_id}/attributes",
            "payload": json.dumps({
                "ts": _now_iso(),
                "tenant_id": tenant_id,
                "device_id": device_id,
                "key": attr_name,
                "value": header[header_key],
            }),
        })
    if attr_msgs:
        mqtt_publish.multiple(attr_msgs, hostname=broker_host(broker), port=broker_port(broker))
        print(f"published {len(attr_msgs)} attributes")

    # --- serie temporale: un messaggio telemetria per riga, timestamp
    # ricostruito da TIME (s) ancorato a "adesso" (il file non contiene un
    # orario di inizio trattamento reale, solo secondi trascorsi) ---
    if not rows:
        print("nessuna riga di tabella trovata, solo attributi caricati")
        return

    times = [leading_number(r.get("TIME (s)", "")) for r in rows]
    times = [t for t in times if t is not None]
    if not times:
        print("colonna TIME (s) illeggibile, salto la serie temporale")
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
            "topic": f"farsight/{tenant_id}/{device_id}/telemetry",
            "payload": json.dumps({
                "ts": ts,
                "tenant_id": tenant_id,
                "device_id": device_id,
                "metrics": metrics,
            }),
        })

    mqtt_publish.multiple(data_msgs, hostname=broker_host(broker), port=broker_port(broker))
    print(f"published {len(data_msgs)} time-series samples")


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
    if len(sys.argv) != 4:
        print(f"usage: {sys.argv[0]} <file.dat> <tenant_id> <device_id>", file=sys.stderr)
        sys.exit(1)

    file_path, tenant_id, device_id = sys.argv[1], sys.argv[2], sys.argv[3]
    import_file(file_path, read_broker_from_server_conf(), tenant_id, device_id)
