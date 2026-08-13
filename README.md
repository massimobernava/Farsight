# Farsight

![build](https://github.com/massimobernava/Farsight/actions/workflows/build-deb.yml/badge.svg)

Piattaforma di accesso remoto (desktop + SSH) e telemetria per un parco di macchine Ubuntu
su VPN mesh Tailscale. Design completo e razionale delle scelte: [PROJECT_SPEC.md](PROJECT_SPEC.md).
Istruzioni operative per Claude Code: [CLAUDE.md](CLAUDE.md).

Stato: prototipo. Client e server sono validati end-to-end in ambiente Docker con join
reale al tailnet (vedi sotto), non ancora su hardware di produzione.

## Struttura repo

```
cmd/farsight-agent/     binario client: telemetria MQTT
cmd/farsight-server/    binario server: control plane / dashboard
internal/               pacchetti Go condivisi (config, telemetria, registry, ...)
packaging/client/       sorgenti .deb client (systemd unit, wrapper x11vnc/websockify, postinst)
packaging/server/       sorgenti .deb server (systemd unit, netconfig, postinst)
docker/                 Dockerfile + script per ambienti di test locali (systemd-in-docker)
.github/workflows/      CI: build dei due .deb
```

## Requisiti

- Go 1.25+ (solo per build locale fuori Docker; la CI usa la versione da `go.mod`)
- Docker (per gli ambienti di test locali)
- Un account Tailscale (piano free va bene) e un [auth key](https://login.tailscale.com/admin/settings/keys)
  quando si vuole testare il join reale al tailnet

## Build dei pacchetti .deb

### Via CI (consigliato)

Ogni push su `main` compila entrambi i pacchetti in GitHub Actions
(`.github/workflows/build-deb.yml`) e li pubblica come artifact (architettura `amd64`,
quella dei runner GitHub-hosted). Per build `arm64` vedi sotto (Docker locale su Apple Silicon
produce arm64 automaticamente).

### In locale, dentro Docker (per test end-to-end reali)

```bash
# ambiente client: Ubuntu + systemd + Xvfb + tailscale + x11vnc + websockify + novnc
./docker/run-client-test.sh
docker exec -w /workspace farsight-client-test bash packaging/client/build.sh

# ambiente server: Ubuntu + systemd + tailscale + mosquitto + telegraf + influxdb2 + grafana
./docker/run-server-test.sh
docker exec -w /workspace farsight-server-test bash packaging/server/build.sh
```

I `.deb` finiscono in `dist/` dentro ogni container (montato dalla working directory del
repo, quindi visibili anche fuori Docker in `./dist/`).

## Installazione (macchina reale o container di test)

### Client

```bash
export TS_AUTHKEY=tskey-auth-...       # opzionale: se assente, join manuale con `tailscale up`
dpkg -i farsight-client_*.deb
```

L'installer (`packaging/client/debian/postinst`) è idempotente:
- se Tailscale è già installato e autenticato, non lo tocca;
- se manca, lo installa e lo autentica con `TS_AUTHKEY` se presente nell'ambiente;
- x11vnc e websockify vengono bindati **solo** sull'IP Tailscale della macchina (mai `0.0.0.0`),
  risolto dinamicamente a ogni avvio del servizio.

Poi modifica `/etc/farsight/client.conf` (in particolare `MQTT_BROKER` con l'IP Tailscale del
server, e `TENANT_ID`/`DEVICE_ID` per identificare la macchina — vedi sotto) e riavvia:

```bash
systemctl restart farsight-agent farsight-x11vnc farsight-vnc-proxy
```

### Server

```bash
export TS_AUTHKEY=tskey-auth-...       # stesso discorso del client
dpkg -i farsight-server_*.deb
```

Il postinst (`packaging/server/debian/postinst`) installa/rileva idempotentemente Mosquitto,
Telegraf, InfluxDB 2.x e Grafana (aggiungendo i repository apt ufficiali InfluxData/Grafana solo
se i pacchetti non sono già presenti), esegue il setup iniziale di InfluxDB (org `farsight`,
bucket `telemetry`, token salvato in `/etc/farsight/influx_token`), e configura tutto per
essere raggiungibile solo sull'interfaccia Tailscale.

## Come un client invia dati al server (MQTT)

1. **Telemetria periodica** (`farsight-agent`, ogni `PUBLISH_INTERVAL_SECONDS`): pubblica un
   JSON su `farsight/<tenant_id>/<device_id>/telemetry` — schema in
   [`internal/telemetry/telemetry.go`](internal/telemetry/telemetry.go) (`cpu_percent`,
   `mem_percent`, `disk_percent`, `tailscale_ip`, stato dei servizi locali...).
2. **Stato online/offline**: retained message su `farsight/<tenant_id>/<device_id>/status`,
   guidato dal Last Will MQTT — se l'agent muore senza disconnessione pulita, il broker stesso
   marca la macchina offline.
3. Sul server, **Telegraf** si iscrive a `farsight/+/+/telemetry` e scrive su InfluxDB
   (config generata da `packaging/server/usr/lib/farsight/server-netconfig.sh`); il
   **control plane** (`farsight-server`) si iscrive a entrambi i topic e tiene una vista in
   memoria di ogni macchina, senza bisogno di database proprio (i retained message della
   MQTT sono già la persistenza).

`tenant_id`/`device_id` sono gli unici campi che identificano una macchina lato dashboard e
Grafana — si impostano in `/etc/farsight/client.conf` (`TENANT_ID`, `DEVICE_ID`; quest'ultimo
di default è l'hostname della macchina). Vanno tenuti stabili nel tempo: sono anche i tag
InfluxDB usati per lo storico.

## Consultare i dati

- **Dashboard control plane**: `http://<ip-tailscale-server>:8080/` — lista macchine, stato
  online/offline, link diretto al desktop noVNC di ciascuna (`/api/devices` per JSON).
- **Grafana**: `http://<ip-tailscale-server>:3000/` (primo accesso `admin`/`admin`, poi richiede
  cambio password) — datasource InfluxDB già pronto sul bucket `telemetry`.
- **Desktop remoto**: dal link "Desktop" in dashboard, o direttamente
  `http://<ip-tailscale-client>:6080/vnc.html` — traffico peer-to-peer dentro la VPN, non passa
  mai dal server (vedi principio architetturale in PROJECT_SPEC.md).

Tutti questi indirizzi sono raggiungibili solo da dentro il tailnet.
