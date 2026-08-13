# Farsight C SDK

Pubblica telemetria/status su un server Farsight senza gestire MQTT direttamente.

- `farsight_connect_from_config` / `farsight_connect` — connessione (la prima legge
  `MQTT_BROKER`/`TENANT_ID`/`DEVICE_ID` da `/etc/farsight/client.conf`, così non li scrivi a
  mano se la macchina ha già `farsight-client` installato)
- `farsight_publish_telemetry` — CPU/RAM/disco (gli stessi campi di `farsight-agent`)
- `farsight_publish_metric` — **una metrica custom qualsiasi** (numero pazienti visitati,
  lettura sensore, ...): nessuna configurazione da toccare lato server, il campo compare in
  InfluxDB al primo invio
- `farsight_set_status` / `farsight_disconnect`

Vedi [`farsight.h`](farsight.h) per i dettagli di ogni funzione e [`example.c`](example.c) per
un uso completo.

## Dipendenza

Wrappa [Eclipse Paho MQTT C](https://www.eclipse.org/paho/index.php?page=clients/c/index.php):

```bash
# Ubuntu/Debian
sudo apt-get install libpaho-mqtt-dev
```

## Build

```bash
cc example.c farsight.c -o farsight-example -lpaho-mqtt3c
./farsight-example tcp://<ip-tailscale-server>:1883 default my-device
```

Per integrarlo in un programma esistente: compila `farsight.c` insieme ai tuoi sorgenti,
`#include "farsight.h"`, linka `-lpaho-mqtt3c`.

## Metriche custom

```c
farsight_client *c = farsight_connect_from_config(NULL); /* legge client.conf */
farsight_publish_metric(c, "patients_visited", 7);
farsight_disconnect(c);
```

Il nome (`"patients_visited"`) diventa direttamente il nome del campo in InfluxDB/Grafana —
va bene qualunque stringa alfanumerica con underscore, senza spazi o virgolette (non viene
validata né escaped).
