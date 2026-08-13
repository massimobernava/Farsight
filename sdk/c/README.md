# Farsight C SDK

Pubblica dati su un server Farsight senza gestire MQTT direttamente. Due tipi di dato, per
scelta esplicita di chi chiama — nessuno dei due è "standard" o obbligatorio:

- `farsight_publish_series(client, name, double value)` — **serie temporale**, si accumula in
  InfluxDB nel tempo (CPU, un sensore, un conteggio ripetuto — qualunque cosa vuoi graficare
  contro il tempo). Niente è privilegiato: CPU/RAM/disco sono solo un esempio, il nome è quello
  che scegli tu.
- `farsight_set_attribute(client, key, const char *value)` — **valore puntuale**, sovrascrive
  invece di accumulare (un IP Tailscale, una versione firmware, un flag di stato). Finisce in
  SQLite su `farsight-server`, non in InfluxDB. `value` è sempre una stringa: i numeri vanno
  bene anche come testo ("12.5"), SQLite non tipizza rigidamente le colonne.

Più `farsight_connect_from_config` / `farsight_connect`, `farsight_set_status`,
`farsight_disconnect`. Vedi [`farsight.h`](farsight.h) per i dettagli e [`example.c`](example.c)
per un uso completo.

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

## Esempio

```c
farsight_client *c = farsight_connect_from_config(NULL); /* legge client.conf */

farsight_publish_series(c, "cpu_percent", 12.5);          /* -> InfluxDB, storico */
farsight_publish_series(c, "patients_visited", 7);         /* -> InfluxDB, storico */
farsight_set_attribute(c, "firmware_version", "1.4.2");    /* -> SQLite, stato corrente */

farsight_disconnect(c);
```

Il nome/chiave diventa direttamente il nome del campo (InfluxDB) o della riga (SQLite) — va
bene qualunque stringa alfanumerica con underscore; per gli attributi anche `value` non va
scritto con virgolette o backslash non escapati (non c'è escaping automatico).
