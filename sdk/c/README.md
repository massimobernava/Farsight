# Farsight C SDK

Pubblica telemetria/status su un server Farsight senza gestire MQTT direttamente. Tre funzioni:
`farsight_connect`, `farsight_publish_telemetry`, `farsight_set_status`, più
`farsight_disconnect`. Vedi [`farsight.h`](farsight.h) per i dettagli e
[`example.c`](example.c) per un uso completo.

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
