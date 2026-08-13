# Farsight

![build](https://github.com/massimobernava/Farsight/actions/workflows/build-deb.yml/badge.svg)

*[English version](README.en.md)*

Accesso remoto (desktop + SSH) e monitoraggio per un parco di macchine Ubuntu, raggiungibili
solo dalla tua rete Tailscale — mai esposte pubblicamente.

Stato: prototipo.

## Requisiti

- Le macchine (client e server) devono far parte della stessa rete Tailscale.
- Se una macchina non ha già Tailscale configurato, ti serve una
  [auth key](https://login.tailscale.com/admin/settings/keys) per farla entrare nella rete
  durante l'installazione.

## Installare il server

Un'unica macchina, dedicata al monitoraggio (dashboard + Grafana).

```bash
export TS_AUTHKEY=tskey-auth-...   # solo se questa macchina non è già su Tailscale
dpkg -i farsight-server_*.deb
```

L'ultimo `.deb` si trova nella pagina [Releases](https://github.com/massimobernava/Farsight/releases).

## Installare il client

Su ogni macchina da monitorare/controllare da remoto.

```bash
export TS_AUTHKEY=tskey-auth-...   # solo se questa macchina non è già su Tailscale
dpkg -i farsight-client_*.deb
```

Poi apri `/etc/farsight/client.conf` e imposta:
- `MQTT_BROKER` — IP Tailscale del server (es. `tcp://100.x.x.x:1883`)
- `TENANT_ID` / `DEVICE_ID` — nome che identifica questa macchina in dashboard (`DEVICE_ID`
  di default è l'hostname; va bene lasciarlo se già parlante)

Poi riavvia:

```bash
systemctl restart farsight-agent farsight-x11vnc farsight-vnc-proxy
```

## Uso

- **Dashboard**: `http://<ip-tailscale-server>:8080/` — elenco macchine, stato online/offline,
  link diretto al desktop remoto di ciascuna.
- **Grafana**: `http://<ip-tailscale-server>:3000/` (primo accesso `admin`/`admin`, poi richiede
  cambio password) — storico delle metriche.

Entrambi raggiungibili solo da dentro la rete Tailscale.

## Documentazione

- [PROJECT_SPEC.md](PROJECT_SPEC.md) — design e scelte architetturali (italiano)
- [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) — build, test, release, per chi sviluppa Farsight
