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

- **Stato live** (solo online/offline + "ultimo visto"): in RAM in `internal/registry`, mai
  persistito — arriva da MQTT (retained per lo status), non ha senso duplicarlo su disco. Il
  registry non tocca più telemetria/metriche: quelle sono solo affare di Telegraf→InfluxDB, il
  control plane non le legge né le mostra (è la dashboard/Grafana a farlo).
- **Identità e attributi persistenti per macchina** (nome visualizzato, note, e attributi
  puntuali come IP Tailscale/raggiungibilità): `internal/store`, SQLite (`modernc.org/sqlite`,
  driver puro Go — niente cgo, stessa scelta di binario statico fatta per tutto il resto). File
  in `/var/lib/farsight/farsight.db`, creato al primo avvio di `farsight-server`.
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
- **Elenco macchine in Grafana: iframe, non pannello tabella nativo.** Un primo tentativo con
  pannello tabella nativo (datasource SQLite) non renderizzava, e non c'è modo di verificarlo da
  qui (nessun accesso browser in questo ambiente di sviluppo — solo l'API `/api/ds/query`, che
  verifica la query, non il rendering del pannello). Soluzione per l'elenco macchine: un
  pannello Text/HTML con `<iframe src="http://<ip>:8080/">` — verificabile end-to-end via `curl`
  contro `farsight-server` stesso. `GET /?device=<id>` filtra a una sola macchina (usato dal
  pannello "Macchina" via `${device_id}`, interpolato da Grafana nell'URL dell'iframe).
- **Dashboard "Farsight - Trattamenti"** (esempio, non provisionata dal pacchetto — vedi
  `examples/ophthalmic-tid-import/`): menu a tendina su `record_id`/paziente (query variabile
  Grafana su `device_records` via SQLite, alias `__value`/`__text`), pannello dettaglio via
  `json_extract(data_json, '$.campo')`, grafici InfluxDB filtrati sullo stesso `record_id`. Qui
  **sì** un pannello tabella nativo — a differenza dell'elenco macchine sopra, non ancora
  confermato visivamente (stesso limite: solo verificato via `/api/ds/query`, non il rendering
  reale). Se non dovesse renderizzare, stessa soluzione: iframe verso una pagina servita da
  `farsight-server`.

## Schema dati / integrare un publisher custom

Il contratto client→server è solo "pubblica JSON su MQTT ai topic giusti" — non serve per
forza far girare `farsight-agent`. Tre topic distinti, per tre tipi di dato diversi (nessuno è
"standard" o obbligatorio oltre all'identità):

- `farsight/<tenant_id>/<device_id>/telemetry` — **serie temporale**, va in InfluxDB e si
  accumula nel tempo. Schema in [`internal/telemetry/telemetry.go`](../internal/telemetry/telemetry.go)
  (`Payload`): solo `ts`/`tenant_id`/`device_id` sono a livello fisso, tutto il resto vive
  dentro `metrics` (oggetto JSON libero, `{"nome": valore, ...}`). Telegraf lo scompone
  automaticamente — ogni chiave diventa un campo InfluxDB a sé, **senza toccare nessuna config
  server** per aggiungerne una nuova (vedi `packaging/server/usr/lib/farsight/server-netconfig.sh`).
  Nemmeno CPU/RAM/disco sono privilegiati: `farsight-agent` li manda perché è la sua funzione,
  ma sono solo voci come le altre dentro `metrics`, e sono disattivabili (`PUBLISH_TELEMETRY=false`
  in `client.conf`). `RecordID` (opzionale, tag InfluxDB) correla una serie con un record
  specifico — vedi sotto.
- `farsight/<tenant_id>/<device_id>/attributes` — **valore puntuale**, va in SQLite
  (`internal/store`, tabella `devices`) e sovrascrive invece di accumulare (`Attribute` in
  telemetry.go: `key`/`value`, sempre stringa). Per fatti sullo **stato corrente della
  macchina**: IP Tailscale, se il desktop è raggiungibile, versione firmware — mai qualcosa che
  si ripete nel tempo e di cui vuoi tenere ogni occorrenza (per quello c'è Record, sotto).
  `farsight-agent` pubblica qui `tailscale_ip` e un unico `desktop_available` (true solo se sia
  x11vnc che websockify sono su — niente booleani tecnici separati esposti).
- `farsight/<tenant_id>/<device_id>/records` — **occorrenza**, va in SQLite (tabella
  `device_records`) e **accumula** — un record per `record_id`, mai sovrascritto da un altro
  (`Record` in telemetry.go: `record_id` + `data`, oggetto JSON libero). Per cose che succedono
  più volte nella vita di un device e di cui serve lo storico: un trattamento, una visita, un
  paziente diverso ogni volta — un attributo sovrascriverebbe l'occorrenza precedente, un record
  no. Un device tratta più di un paziente: non ha senso questo sistema senza uno storico
  multi-trattamento, quindi questo canale esiste apposta. Publicare due volte lo stesso
  `record_id` sostituisce quel record (retry idempotente), non crea una seconda voce.
- `farsight/<tenant_id>/<device_id>/status` — retained, `online`/`offline` (guidato dal Last
  Will MQTT: se il processo muore senza disconnessione pulita, il broker stesso marca la
  macchina offline — non serve ripubblicarlo periodicamente).

`tenant_id`/`device_id` restano gli unici identificatori richiesti su ogni topic: non serve un
quarto ID per "che tipo di dato è", basta scegliere il topic giusto.

**Da CLI**, con `mosquitto_pub` (pacchetto `mosquitto-clients`):

```bash
mosquitto_pub -h <ip-tailscale-server> -t 'farsight/default/mia-macchina/status' -r -q 1 -m 'online'

# serie temporale
mosquitto_pub -h <ip-tailscale-server> -t 'farsight/default/mia-macchina/telemetry' -q 1 -m \
  '{"ts":"2026-08-13T12:00:00Z","tenant_id":"default","device_id":"mia-macchina","metrics":{"cpu_percent":12.5,"patients_visited":7}}'

# valore puntuale
mosquitto_pub -h <ip-tailscale-server> -t 'farsight/default/mia-macchina/attributes' -q 1 -m \
  '{"ts":"2026-08-13T12:00:00Z","tenant_id":"default","device_id":"mia-macchina","key":"firmware_version","value":"1.4.2"}'

# occorrenza (accumula, non sovrascrive)
mosquitto_pub -h <ip-tailscale-server> -t 'farsight/default/mia-macchina/records' -q 1 -m \
  '{"ts":"2026-08-13T12:00:00Z","tenant_id":"default","device_id":"mia-macchina","record_id":"TREATMENT-042","data":{"patient_id":"PID-042"}}'
```

**Da C**, con l'SDK in [`sdk/c/`](../sdk/c/) — wrapper minimo sopra Eclipse Paho MQTT C e
libcurl (`libpaho-mqtt-dev libcurl4-openssl-dev` su Ubuntu) che nasconde MQTT/HTTP dietro poche
funzioni con nomi propri, incluse lettura automatica di tenant/device da `client.conf`
(dettagli nel [README dell'SDK](../sdk/c/README.md)):

```c
/* cc example.c farsight.c -o farsight-example -lpaho-mqtt3c -lcurl */
#include "farsight.h"

int main(void) {
    farsight_client *c = farsight_connect_from_config(NULL); /* legge client.conf */
    if (!c) return 1;

    farsight_publish_series(c, "cpu_percent", 12.5);              /* -> InfluxDB, storico */
    farsight_set_attribute_string(c, "firmware_version", "1.4.2"); /* -> SQLite, stato corrente */

    farsight_field fields[] = {{"patient_id", "PID-042"}};
    farsight_publish_record(c, "TREATMENT-042", fields, 1);        /* -> SQLite, accumula */

    farsight_upload_file(c, 0, "/path/to/report.dat");             /* -> file sul server */

    farsight_disconnect(c); /* pubblica anche status=offline, pulito */
    return 0;
}
```

`farsight_connect` pubblica subito `status=online` e imposta un Last Will MQTT su
`status=offline` — non c'è (né serve) una funzione pubblica per dichiararlo a mano: la
connessione riuscita/persa È lo stato, stessa garanzia di `farsight-agent`. Compilato ed
eseguito davvero contro il control plane, non solo scritto a mano — vedi
[`sdk/c/example.c`](../sdk/c/example.c).

## Upload file e importer

Canale HTTP separato da MQTT — per file interi, non singoli valori (backup, dataset grossi,
formati proprietari che un dispositivo produce già così):

```
POST /devices/<tenant_id>/<device_id>/upload?filename=<nome>
```

Corpo della richiesta = contenuto grezzo del file. Il server lo salva sotto
`UPLOAD_DIR/<tenant>/<device>/<timestamp>-<nome>` (`server.conf`, default
`/var/lib/farsight/uploads`). Da C: `farsight_upload_file(client, http_port, path)` nell'SDK
ufficiale (`sdk/c/`) — non serve gestire HTTP a mano, l'host è derivato dalla stessa connessione
MQTT già aperta.

**`farsight-server` non interpreta mai il contenuto del file.** Dopo il salvataggio, cerca uno
script eseguibile in `IMPORTERS_DIR/<estensione-senza-punto>` (default
`/etc/farsight/importers/`, es. `IMPORTERS_DIR/dat` per un file `.dat`) e, se esiste, lo lancia:

```
<script> <percorso-file-salvato> <tenant_id> <device_id>
```

Nessun importer configurato per un'estensione → il file resta semplicemente salvato, nessun
errore. Lo script è libero di fare qualunque cosa (tipicamente: parsare il file e pubblicare su
MQTT con gli stessi meccanismi di uno qualunque publisher, vedi sopra) — è un punto di
estensione generico, non una feature legata a un formato specifico. Vedi
[`examples/ophthalmic-tid-import/`](../examples/ophthalmic-tid-import/) per un importer completo
e funzionante (formato proprietario di un dispositivo oftalmico, deliberatamente **non**
installato da questo pacchetto — proprietario/site-specific, non appartiene al prodotto base).
