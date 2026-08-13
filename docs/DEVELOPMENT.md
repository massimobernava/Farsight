# Sviluppo di Farsight

Documento per chi sviluppa/mantiene Farsight, non per chi lo installa e basta — per quello
vedi il [README](../README.md). Design e scelte architetturali: [PROJECT_SPEC.md](../PROJECT_SPEC.md).

## Struttura repo

```
cmd/farsight-agent/     binario client: telemetria MQTT
cmd/farsight-server/    binario server: control plane (backend + pagina azioni di scrittura)
internal/               pacchetti Go condivisi (config, telemetria, registry, store, ...)
sdk/c/                  SDK C per integrare Farsight senza gestire MQTT direttamente
packaging/client/       sorgenti .deb client (systemd unit, wrapper x11vnc/websockify, postinst)
packaging/server/       sorgenti .deb server (systemd unit, netconfig, postinst)
docker/                 Dockerfile + script per ambienti di test locali (systemd-in-docker)
.github/workflows/      CI: build continua + release
```

## Requisiti

- Go 1.25+ (solo per build locale fuori Docker; CI/release usano la versione da `go.mod`)
- Docker (per gli ambienti di test locali)
- Un account Tailscale (piano free va bene) e un [auth key](https://login.tailscale.com/admin/settings/keys)
  per testare il join reale al tailnet

## Ambiente di test locale (Docker)

Gli installer toccano systemd, Tailscale, x11vnc/websockify/mosquitto/influxdb/grafana —
troppo rischioso validarli solo a occhio. `docker/` fornisce due container Ubuntu con systemd
reale (non solo processi sciolti), pensati per riprodurre una macchina vera:

```bash
# ambiente client: Ubuntu + systemd + Xvfb + tailscale + x11vnc + websockify + novnc
./docker/run-client-test.sh
docker exec -w /workspace farsight-client-test bash packaging/client/build.sh
docker exec -e TS_AUTHKEY=<authkey> farsight-client-test dpkg -i /workspace/dist/farsight-client_*.deb

# ambiente server: Ubuntu + systemd + tailscale + mosquitto + telegraf + influxdb2 + grafana
./docker/run-server-test.sh
docker exec -w /workspace farsight-server-test bash packaging/server/build.sh
docker exec -e TS_AUTHKEY=<authkey> farsight-server-test dpkg -i /workspace/dist/farsight-server_*.deb
```

I due container, joinati sullo stesso tailnet reale, comunicano tra loro via IP Tailscale
esattamente come farebbero due macchine vere — è così che si è validata la pipeline
telemetria → MQTT → Telegraf → InfluxDB → Grafana end-to-end, ed è così che sono stati trovati
due bug reali prima che finissero su una macchina vera: x11vnc apriva comunque un listener
IPv6 wildcard nonostante `-listen <ip>` (serve `-rfbportv6 -1`), e InfluxDB bindava di default
su tutte le interfacce invece che solo loopback.

## Build dei pacchetti .deb

`packaging/client/build.sh` e `packaging/server/build.sh` compilano il binario Go
(`CGO_ENABLED=0`, arch dell'host via `dpkg --print-architecture`), assemblano l'albero
`DEBIAN/` + file di pacchetto, e lanciano `dpkg-deb --build`. Variabile `VERSION` opzionale
(default `0.1.0`). Output in `dist/`.

## CI

- **`build-deb.yml`**: ogni push su `main` builda entrambi i `.deb` e li carica come *artifact
  della run* (tab Actions del repo, in fondo alla pagina della run, zip scaricabile, scade dopo
  90 giorni). Non è una Release GitHub.
- **`release.yml`**: manuale (tab Actions → "Build Release" → *Run workflow*, inserisci una
  versione tipo `v0.1.0`). Builda entrambi i `.deb` con quella versione, crea il tag, e apre una
  **Release GitHub in bozza** con i due `.deb` allegati (tab Releases) — la pubblichi a mano
  dopo averla controllata.

## Control plane e Grafana

Decisione architetturale (vedi PROJECT_SPEC.md "Componente 2"): **Grafana è l'interfaccia
principale**, `farsight-server` non cerca di diventare un motore di dashboard proprio — sarebbe
reinventare male qualcosa che Grafana già fa bene (editor pannelli, variabili template,
multi-tenant via Organizations).

- **Stato live** (online/offline, ultime metriche): solo in RAM in `internal/registry`, mai
  persistito — arriva da MQTT (retained per lo status), non ha senso duplicarlo su disco.
- **Identità persistente per macchina** (nome visualizzato, note): `internal/store`, SQLite
  (`modernc.org/sqlite`, driver puro Go — niente cgo, stessa scelta di binario statico fatta
  per tutto il resto). File in `/var/lib/farsight/farsight.db`, creato al primo avvio di
  `farsight-server`.
- **Grafana legge lo stesso file SQLite direttamente** (plugin `frser-sqlite-datasource`,
  installato idempotentemente dal postinst) — non passa dal control plane per leggere
  l'anagrafica, query SQL dirette.
- **Le uniche scritture** (rinominare una macchina, per ora) passano da un form HTML minimale
  servito da `farsight-server` stesso (`POST /devices/{tenant}/{device}/rename`) — pensato per
  essere incorporato in Grafana come **pannello iframe**, non per essere usato come pagina a sé.
  Grafana è debole in CRUD, non vale la pena costruirci un plugin vero sopra per una form del
  genere.
- **Datasource Grafana provisionate da file**, non da chiamate API a mano (`postinst` scrive
  `/etc/grafana/provisioning/datasources/farsight.yaml` con token InfluxDB e path SQLite già
  compilati) — idempotente, un vero install non ha un admin che clicca nella UI di Grafana.

## Schema telemetria / integrare un publisher custom

Il contratto client→server è solo "pubblica JSON su MQTT ai topic giusti" — non serve per
forza far girare `farsight-agent`: utile per integrare un programma esistente che vuole
mandare i propri dati senza un processo esterno.

- `farsight/<tenant_id>/<device_id>/telemetry` — JSON, schema in
  [`internal/telemetry/telemetry.go`](../internal/telemetry/telemetry.go) (`Payload`). Solo
  `ts`/`tenant_id`/`device_id` sono obbligatori lato Telegraf (`optional = true` su tutto il
  resto — vedi `packaging/server/usr/lib/farsight/server-netconfig.sh`); un publisher può
  mandarne un sottoinsieme qualsiasi.
- `farsight/<tenant_id>/<device_id>/status` — retained, `online`/`offline` (nell'agent è
  guidato dal Last Will MQTT: se il processo muore senza disconnessione pulita, il broker
  stesso marca la macchina offline).
- **Metriche custom**: campo `metrics` (oggetto JSON libero, `{"nome": valore, ...}`) dentro
  lo stesso payload di telemetria — per dati specifici dell'applicazione che non sono CPU/RAM/
  disco (es. numero pazienti visitati, letture sensori). Telegraf lo scompone automaticamente:
  ogni chiave diventa un campo InfluxDB a sé, **senza toccare nessuna config server** per
  aggiungerne una nuova. Il nome della chiave è quello che finisce in InfluxDB/Grafana — va
  scelto con cura, non c'è validazione. `tenant_id`/`device_id` restano gli unici identificatori
  richiesti: non serve un terzo ID per "che tipo di dato è", basta il nome della metrica.

**Da CLI**, con `mosquitto_pub` (pacchetto `mosquitto-clients`):

```bash
mosquitto_pub -h <ip-tailscale-server> -t 'farsight/default/mia-macchina/status' -r -q 1 -m 'online'

mosquitto_pub -h <ip-tailscale-server> -t 'farsight/default/mia-macchina/telemetry' -q 1 -m \
  '{"ts":"2026-08-13T12:00:00Z","tenant_id":"default","device_id":"mia-macchina","cpu_percent":12.5,"mem_percent":40.2,"disk_percent":55.0,"service_x11vnc_up":true,"service_websockify_up":true}'
```

**Da C**, con l'SDK in [`sdk/c/`](../sdk/c/) — wrapper minimo sopra Eclipse Paho MQTT C
(`libpaho-mqtt-dev` su Ubuntu) che nasconde MQTT dietro poche funzioni con nomi propri, incluse
metriche custom e lettura automatica di tenant/device da `client.conf` (dettagli nel
[README dell'SDK](../sdk/c/README.md)):

```c
/* cc example.c farsight.c -o farsight-example -lpaho-mqtt3c */
#include "farsight.h"

int main(void) {
    farsight_client *c = farsight_connect_from_config(NULL); /* legge client.conf */
    if (!c) return 1;

    farsight_publish_telemetry(c, 12.5 /* cpu% */, 40.2 /* mem% */, 55.0 /* disk% */);
    farsight_publish_metric(c, "patients_visited", 7); /* metrica custom, qualunque nome */

    farsight_disconnect(c); /* pubblica anche status=offline, pulito */
    return 0;
}
```

`farsight_connect` pubblica subito `status=online` e imposta un Last Will MQTT su
`status=offline` (stessa garanzia di `farsight-agent`: se il processo muore senza
disconnessione pulita, il broker marca comunque la macchina offline). Compilato ed eseguito
davvero contro il control plane, non solo scritto a mano — vedi [`sdk/c/example.c`](../sdk/c/example.c).
