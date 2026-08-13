# Farsight — Specifica di progetto

## Obiettivo

Farsight è una piattaforma di accesso remoto (desktop + SSH) e telemetria
per un parco di
macchine Ubuntu collegate in VPN mesh, pensata inizialmente per un contesto
IoT/industriale-medicale (dispositivi che eseguono software LabVIEW e
necessitano di supervisione/debug remoto). Alternativa più stabile e
controllata a TeamViewer.

Il prodotto finale è pensato anche per essere rivendibile come servizio in
abbonamento a terzi (multi-tenant), ma la fase attuale è un **prototipo /
proof of concept** su infrastruttura reale già esistente (alcune macchine
hanno già Tailscale installato e configurato: l'installer non deve
distruggere configurazioni esistenti).

---

## Architettura generale

```
┌─────────────────────────────────────────────────────────────┐
│  VPN mesh (Tailscale, in futuro migrabile a Headscale)       │
│                                                                │
│   ┌───────────────┐        ┌───────────────┐                │
│   │  Client N      │        │  Server        │                │
│   │  (macchina     │        │  (dashboard +  │                │
│   │  Ubuntu con    │◄──────►│  telemetria)   │                │
│   │  LabVIEW)      │  MQTT  │                │                │
│   └───────────────┘        └───────────────┘                │
│          ▲                                                    │
│          │ HTTP diretto (noVNC) / SSH diretto                │
│          │ (NON passa dal server, peer-to-peer via VPN)      │
│   ┌───────────────┐                                          │
│   │  Browser       │                                          │
│   │  operatore     │                                          │
│   │  (dentro VPN)  │                                          │
│   └───────────────┘                                          │
└─────────────────────────────────────────────────────────────┘
```

Principio chiave: il **server centrale NON fa da proxy per il traffico di
desktop remoto o SSH**. Fa solo da:
1. elenco/dashboard delle macchine online (via telemetria MQTT + stato
   Tailscale),
2. controllo permessi/tenant,
3. generazione dinamica dei link diretti verso l'IP VPN della macchina
   target.

Il traffico VNC/SSH viaggia peer-to-peer dentro il tunnel WireGuard
(Tailscale), direttamente tra il browser dell'operatore e la macchina
target.

---

## Convenzioni di naming

- Repository: `farsight`
- Pacchetto client: `farsight-client.deb`
- Pacchetto server: `farsight-server.deb`
- Servizi systemd: `farsight-agent` (telemetria), `farsight-vnc-proxy`
  (websockify + noVNC), `farsight-server` (control plane applicativo)
- Config file client: `/etc/farsight/client.conf`
- Config file server: `/etc/farsight/server.conf`

---

## Componente 1 — Pacchetto Client (.deb)

Da installare su ogni macchina Ubuntu da monitorare/controllare.

### Contenuto

- **Tailscale**: se già installato e configurato (caso comune nei test
  attuali), l'installer **deve rilevarlo e non toccarlo/reinstallarlo/
  riconfigurarlo**. Se non presente, lo installa e lo configura con
  `--login-server` puntato al control server (Tailscale ufficiale in questa
  fase, Headscale in futuro — parametrizzare l'URL).
- **x11vnc**: server VNC che si attacca alla sessione X11 esistente (non
  crea sessioni dedicate). Deve essere bindato **solo sull'interfaccia
  Tailscale** (`-listen <ip-tailscale-locale>`), mai su `0.0.0.0`.
  - Rilevare dinamicamente l'IP Tailscale della macchina all'avvio del
    servizio (l'IP può cambiare).
- **websockify**: proxy WebSocket↔TCP per far parlare noVNC (browser) con
  x11vnc. Gira localmente sulla stessa macchina, bindato anch'esso solo su
  interfaccia Tailscale.
- **noVNC**: client VNC in puro JS/HTML, servito come pagina statica dallo
  stesso host (es. via un piccolo web server integrato o lo stesso
  websockify che supporta serving di file statici).
- **Agent di telemetria** (`farsight-agent`): piccolo processo che pubblica periodicamente su
  MQTT (broker centrale sul server) via publish:
  - stato online/heartbeat
  - IP Tailscale corrente
  - metriche di sistema di base (CPU, RAM, disco — estendibile in seguito
    a metriche specifiche del processo LabVIEW se necessario)
  - stato dei servizi locali (x11vnc/websockify up/down) — utile per
    sapere dalla dashboard se la macchina è "online in VPN" ma con VNC
    non raggiungibile
- **Registrazione al server**: al primo avvio, il client si presenta al
  control plane del server (via API interna, dentro la VPN) con un
  token di provisioning per associarsi al tenant/gruppo corretto.
  In questa fase di prototipo può essere semplificato (es. config file
  con tenant ID statico), ma progettare l'interfaccia pensando alla
  futura automazione.

### Requisiti installer (.deb)

- Deve essere **idempotente**: rieseguibile senza rompere configurazioni
  esistenti (in particolare Tailscale già presente e già autenticato).
- Deve controllare se i servizi (x11vnc, websockify, agent MQTT) sono già
  attivi prima di sovrascriverli.
- Servizi gestiti via `systemd` (unit dedicate, restart automatico).
- SSH: nessun servizio aggiuntivo da installare, si usa l'SSH di sistema
  già presente su Ubuntu, raggiungibile via IP Tailscale. Eventualmente
  prevedere solo un layer di **audit/logging delle sessioni SSH** in una
  fase successiva (fuori scope per il prototipo).

---

## Componente 2 — Pacchetto Server (.deb)

Da installare su una macchina dedicata (anche minimale per il test) sempre
dentro la stessa rete Tailscale.

### Contenuto

- **Mosquitto** (broker MQTT centrale): riceve la telemetria pubblicata da
  tutti i client.
- **InfluxDB** (o TimescaleDB, da valutare in fase implementativa): storage
  time-series per la telemetria.
- **Grafana**: dashboard di visualizzazione della telemetria storica.
- **Control plane applicativo** (`farsight-server`, da sviluppare — è il
  vero cuore del progetto):
  - elenco macchine registrate, stato online/offline (incrociando dati
    MQTT + eventualmente API Tailscale se disponibile)
  - per ogni macchina: link diretto generato dinamicamente a
    `http://<ip-tailscale-macchina>:<porta-websockify>/vnc.html` per
    l'accesso desktop, e indicazione dell'IP per SSH manuale
  - gestione utenti/permessi (chi può vedere/accedere a quali macchine) —
    in fase di prototipo può essere minimale, ma progettato in vista di
    multi-tenant
  - nella fase di prototipo, niente billing/abbonamenti: quello è fuori
    scope per ora

### Requisiti installer (.deb)

- Analogamente al client, idempotente e non distruttivo.
- Deve esporre la dashboard del control plane **solo su interfaccia
  Tailscale**, mai pubblicamente.

---

## Sicurezza / controllo accessi

- **Tutto il sistema vive esclusivamente dentro la VPN mesh.** Nessun
  servizio (x11vnc, websockify, dashboard, Grafana, Mosquitto) deve essere
  esposto su interfacce diverse da quella Tailscale.
- **Controllo accessi granulare**: da implementare tramite **ACL native di
  Tailscale** (o Headscale in futuro) — es. un utente/gruppo può
  raggiungere solo le macchine del proprio tenant/reparto. Questa è la
  strada scelta rispetto a un sistema di token/password temporanee
  generate dal server (opzione scartata per ora, da riconsiderare se
  servirà audit più fine in futuro, es. per requisiti regolatori in
  ambito medicale).
- x11vnc: valutare se abilitare comunque una password VNC di base come
  ulteriore layer, anche se il controllo principale è demandato alla VPN.

---

## Note su VPN: Tailscale vs Headscale

- **Fase attuale (prototipo)**: usare **Tailscale** così com'è (piano
  free, fino a 100 dispositivi), incluse le macchine di test già
  configurate. Zero infrastruttura aggiuntiva da gestire.
- **Fase futura (prodotto commerciale)**: valutare migrazione a
  **Headscale** self-hosted (stesso binario client `tailscale`, cambia
  solo l'endpoint `--login-server`) per:
  - indipendenza da limiti/pricing di un vendor terzo
  - controllo totale su ACL e metadati di rete
  - richiede una VPS con IP pubblico per il control server (coordinamento
    chiavi WireGuard — economica, carico minimo) ed eventualmente un
    relay DERP proprio se non si vogliono usare quelli pubblici di
    Tailscale (che restano comunque cifrati end-to-end anche se usati
    con Headscale).
  - il traffico dati (VNC/SSH/MQTT) NON passa mai dal control server,
    solo la fase di coordinamento iniziale.
- L'URL del control server deve essere **parametrizzato fin da subito**
  nell'installer/config, per rendere la futura migrazione a Headscale un
  semplice cambio di configurazione, non uno sviluppo aggiuntivo.

---

## Roadmap del prototipo (ordine consigliato di sviluppo)

1. Setup manuale su 2 macchine di test (una "client" con LabVIEW
   simulato/reale, una "server") per validare a mano il flusso completo:
   Tailscale (già presente) → x11vnc → websockify → noVNC raggiungibile
   da browser di una terza macchina in VPN.
2. Script/agent di telemetria: publisher MQTT lato client, Mosquitto +
   InfluxDB + Grafana lato server, validare che i dati arrivino e siano
   visualizzabili.
3. Control plane minimale: pagina web che lista le macchine note (anche
   hardcoded in questa fase) con stato online/offline e link diretto al
   noVNC di ciascuna.
4. Impacchettare tutto in .deb (client e server separati), con logica di
   idempotenza/detection di installazioni esistenti.
5. Solo dopo aver validato il prototipo: multi-tenant, provisioning
   automatico, ACL avanzate, eventuale migrazione a Headscale, billing.

---

## Fase 2 — Roadmap futura (assistente LLM locale)

**Non da implementare nel prototipo attuale.** Documentato qui solo per tracciare la
direzione e vincolare le scelte architetturali odierne a non ostacolarla.

### Visione

Il server centrale monterà una **RTX 5090** e ospiterà **Ollama** con un modello locale nella
fascia 30-35B (es. Qwen2.5-32B o simili), per due casi d'uso:

1. **Assistente in linguaggio naturale sulla telemetria**: query su InfluxDB tradotte da NL,
   riepiloghi automatici, rilevamento anomalie descritte testualmente.
2. **Assistente diagnostico**: dato il contesto di un dispositivo (log, metriche, storico),
   aiuta a fare triage prima che un operatore si connetta via VNC.

### Vincolo di design (guida fin da ora)

**Tutto deve girare dentro la VPN, on-prem — nessun dato esce verso API cloud esterne.**
Requisito per il contesto medicale/compliance. Da tenere come principio guida anche se l'LLM
non viene implementato ora (es. non introdurre dipendenze o integrazioni che assumano
chiamate a API cloud esterne per funzionalità equivalenti).

### Implicazioni per le scelte odierne

- Il control plane (`farsight-server`) va strutturato in modo da poter esporre in futuro un
  modulo/endpoint aggiuntivo (es. `farsight-assistant`) senza refactoring pesante — evitare
  quindi un monolite rigido dove aggiungere un nuovo servizio richieda riscritture.
- Lo schema dati InfluxDB/telemetria va progettato con nomi di campo/tag chiari, consistenti e
  semanticamente descrittivi (query-friendly), così che in futuro un LLM possa interrogarlo
  correttamente senza necessità di reinterpretare/rinominare i dati storici.

---

## Fase 2 — Roadmap futura (upload file client→server)

**Non da implementare nel prototipo attuale.** Direzione: sempre la macchina client che
inizia l'invio (push), mai il server/control plane che va a prendere qualcosa sul client —
stesso principio "peer non fa da proxy passivo" già usato per VNC/SSH, qui ribaltato: il
server espone un endpoint, il client ci si connette quando ha qualcosa da mandare.

Due casi d'uso:
1. **Backup** di file dal client verso il server.
2. **Invio dati applicativi**: es. il software medico (LabVIEW) estrae informazioni dal proprio
   DB locale e le invia al server per renderle visibili all'utente tramite Grafana — dati troppo
   grossi o strutturati diversamente da un normale publish periodico via MQTT.

### Vincolo di design

MQTT (usato per la telemetria) **non è adatto** a payload grossi: niente di quel percorso va
riusato per file. Serve un canale HTTP separato, sempre client→server.

### Implicazioni per le scelte odierne

- `farsight-server` ha già un HTTP server bindato solo su interfaccia Tailscale (la dashboard,
  vedi Componente 2): un futuro endpoint tipo `/api/upload` ci si aggiunge senza refactoring,
  stesso principio già seguito per il futuro `farsight-assistant`.
- I file vanno taggati con `tenant_id`/`device_id` (stessi campi già usati in telemetria) per
  poterli attribuire alla macchina di origine.
- Se la libreria/CLI client per l'integrazione in LabVIEW (vedi sopra, binding C via Paho MQTT)
  verrà realizzata, dovrà esporre anche un path di upload HTTP oltre al publish MQTT — non solo
  telemetria a basso volume.
- Dati bulk destinati a Grafana: valutare se instradarli via ingest diretto in InfluxDB (line
  protocol) o come file grezzi con parsing lato server — decisione da prendere quando il caso
  d'uso concreto sarà chiaro, non ora.

---

## Note sul nome

"Farsight" è stato scelto per il doppio richiamo: "vedere lontano" come
concetto di accesso/visione remota del sistema, e come riferimento
diretto all'ambito applicativo del dispositivo medicale (oftalmologia —
"far vision" è terminologia clinica reale). Da verificare, se il progetto
uscirà anche come prodotto commerciale esterno, eventuali conflitti di
marchio/dominio prima dell'uso pubblico definitivo.

---

## Fuori scope per il prototipo attuale

- Billing / gestione abbonamenti
- Provisioning zero-touch automatico
- Audit trail avanzato delle sessioni (oltre al log di base)
- Migrazione a Headscale (solo da tenere presente nel design, non da
  implementare ora)
- Conformità normativa specifica (GDPR/MDR ecc. — da affrontare quando il
  prodotto si avvicina a un uso reale su dati sanitari)
- Assistente LLM locale (Ollama + RTX 5090) — vedi sezione "Fase 2 — Roadmap
  futura" sopra: non implementare ora, ma tenerne conto nelle scelte
  architetturali del control plane e dello schema telemetria
- Upload file client→server (backup, dati bulk per Grafana) — vedi sezione
  "Fase 2 — Roadmap futura (upload file client→server)" sopra: non
  implementare ora, ma non chiudere l'HTTP server del control plane in modo
  che aggiungerlo dopo richieda un refactoring
