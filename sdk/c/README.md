# Farsight C SDK

Pubblica dati su un server Farsight senza gestire MQTT o HTTP direttamente. Tre tipi di dato,
per scelta esplicita di chi chiama — nessuno è "standard" o obbligatorio:

- `farsight_publish_series(client, name, double value)` — **serie temporale**, si accumula in
  InfluxDB nel tempo (CPU, un sensore, una lettura ripetuta).
- `farsight_set_attribute_string` / `farsight_set_attribute_double(client, key, value)` —
  **valore puntuale**, sovrascrive invece di accumulare (un IP Tailscale, una versione
  firmware). Finisce in SQLite, non InfluxDB.
- `farsight_publish_record(client, record_id, fields, field_count)` — **occorrenza**, accumula
  come una serie temporale ma è un intero oggetto invece di un singolo valore (un trattamento,
  una visita, un file caricato). Un dispositivo che serve più pazienti nella sua vita usa questo,
  non gli attributi — un attributo sovrascriverebbe il paziente precedente.

Più `farsight_upload_file(client, http_port, path)` — carica un file intero sul server (backup,
dati troppo grossi per serie/record). E `farsight_connect_from_config` / `farsight_connect`,
`farsight_disconnect`.

Vedi [`farsight.h`](farsight.h) per i dettagli di ogni funzione e [`example.c`](example.c) per
un uso completo. Per un esempio reale end-to-end (upload di un file, un importer server-side che
lo interpreta, dati che finiscono in Grafana con un menu a tendina per paziente):
[`examples/ophthalmic-tid-import/`](../../examples/ophthalmic-tid-import/).

## Dipendenze

```bash
# Ubuntu/Debian
sudo apt-get install libpaho-mqtt-dev libcurl4-openssl-dev
```

## Build

```bash
cc example.c farsight.c -o farsight-example -lpaho-mqtt3c -lcurl
./farsight-example tcp://<ip-tailscale-server>:1883 default my-device
```

Per integrarlo in un programma esistente: compila `farsight.c` insieme ai tuoi sorgenti,
`#include "farsight.h"`, linka `-lpaho-mqtt3c -lcurl`.

## Esempio

```c
farsight_client *c = farsight_connect_from_config(NULL); /* legge client.conf */

farsight_publish_series(c, "cpu_percent", 12.5);            /* -> InfluxDB, storico */
farsight_set_attribute_string(c, "firmware_version", "1.4.2"); /* -> SQLite, stato corrente */

farsight_field fields[] = {
    {"patient_id", "PID-001"},
    {"outcome", "success"},
};
farsight_publish_record(c, "TREATMENT-001", fields, 2);      /* -> SQLite, accumula */

farsight_upload_file(c, 0, "/path/to/report.dat");           /* -> file salvato sul server */

farsight_disconnect(c);
```

Nomi/chiavi diventano direttamente nomi di campo (InfluxDB) o colonne JSON (SQLite) — va bene
qualunque stringa alfanumerica con underscore; valori/chiavi non vanno scritti con virgolette o
backslash non escapati (non c'è escaping automatico).
