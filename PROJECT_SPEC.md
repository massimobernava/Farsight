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
  MQTT (broker centrale sul server), su due topic distinti — nessuno dei due obbligatorio,
  nemmeno per l'agent stesso:
  - **stato online/offline**: retained + Last Will MQTT, gestito automaticamente dalla
    connessione (nessun heartbeat periodico da ripubblicare — il broker marca offline appena
    la connessione cade)
  - **attributi puntuali** (sovrascrivono, non si accumulano): IP Tailscale corrente,
    `desktop_available` (true solo se sia x11vnc che websockify sono su — un solo stato
    "raggiungibile o no", non due booleani tecnici separati)
  - **telemetria/serie temporale** (opzionale, `PUBLISH_TELEMETRY` in `client.conf`): metriche
    di sistema di base (CPU, RAM, disco — estendibile in seguito a metriche specifiche del
    processo LabVIEW). Nessun campo è "di serie": anche CPU/RAM/disco sono solo voci in un bag
    aperto di metriche, non campi fissi nello schema — vedi "Fase 2" più sotto per il razionale
    e per come un publisher qualsiasi (SDK C, script) può aggiungerne di custom senza toccare
    configurazione server
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
- **InfluxDB**: storage time-series per la telemetria, alimentato da
  **Telegraf** che fa da bridge MQTT→InfluxDB (nessun codice di ingestion
  nostro).
- **SQLite** (`/var/lib/farsight/farsight.db`): anagrafica macchine
  persistente — nome visualizzato, note — scritta da `farsight-server`,
  letta anche direttamente da Grafana (plugin datasource SQLite) per dati
  non a serie temporale (es. futuri record strutturati da upload file).
- **Grafana è l'interfaccia principale**, non solo storico telemetria —
  decisione esplicita (non il control plane applicativo a costruire un
  proprio motore di dashboard, sarebbe reinventare Grafana):
  - **Organizations** Grafana per il multi-tenant (un cliente = una org,
    isolamento dashboard/permessi/datasource nativo)
  - dashboard per macchina via variabile template (`device_id`),
    modificabile liberamente dall'utente finale (ruolo Editor) — nessun
    editor di pannelli da costruire
  - dati non a serie temporale (es. record strutturati) via datasource
    SQLite che legge lo stesso file scritto dal control plane
  - futuro assistente LLM (vedi Fase 2): pannello/plugin Grafana, stesso
    principio di "un'unica interfaccia"
- **Control plane applicativo** (`farsight-server`): non è più pensato come
  l'interfaccia principale, ma come backend + superficie per le azioni di
  scrittura che Grafana (tool di visualizzazione, debole in CRUD) non
  copre bene:
  - elenco macchine, stato online/offline (da MQTT, nessun DB per questo:
    i retained message del broker sono già la persistenza)
  - anagrafica persistente per macchina (nome, note) in SQLite, modificabile
    da un form nella pagina del control plane
  - link diretto generato dinamicamente a
    `http://<ip-tailscale-macchina>:<porta-websockify>/vnc.html` per
    l'accesso desktop, e indicazione dell'IP per SSH manuale
  - pensata per essere incorporata dentro Grafana (pannello iframe) così
    l'utente finale resta sempre "dentro Grafana" — non serve sviluppo di
    plugin Grafana vero e proprio per questo, un iframe basta
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

## Upload file client→server + importer per estensione

**Implementato** (era pianificato come Fase 2, realizzato prima del previsto perché è servito
per un caso reale — vedi sotto). Direzione: sempre la macchina client che inizia l'invio
(push), mai il server/control plane che va a prendere qualcosa sul client — stesso principio
"peer non fa da proxy passivo" già usato per VNC/SSH, qui ribaltato: il server espone un
endpoint, il client ci si connette quando ha qualcosa da mandare.

`POST /devices/<tenant_id>/<device_id>/upload?filename=<nome>` su `farsight-server` (stesso
HTTP server della dashboard, bindato solo su Tailscale). MQTT non è adatto a payload grossi,
quindi canale HTTP separato, come previsto.

**`farsight-server` non interpreta mai il contenuto del file.** Dopo il salvataggio cerca uno
script eseguibile in `IMPORTERS_DIR/<estensione>` e, se esiste, lo lancia con
`(percorso-file, tenant_id, device_id)` — punto di estensione generico per formato, non
un parser specifico nel codice base. Dettagli in
[docs/DEVELOPMENT.md](docs/DEVELOPMENT.md#upload-file-e-importer).

Caso reale che ha guidato il design: un dispositivo oftalmico del parco macchine produce un
formato `.dat` proprietario e non standardizzabile (header chiave:valore + tabella TSV — vedi
[`examples/ophthalmic-tid-import/`](examples/ophthalmic-tid-import/), non installato dal
pacchetto, deliberatamente fuori dal prodotto base). L'importer di esempio dimostra il mapping:
valori puntuali (ID paziente, parametri intervento) → attributi → SQLite; tabella → telemetria
→ InfluxDB — stessi due canali di qualunque publisher, nessuna feature nuova nel core.

**Limite noto, non risolto**: lo schema attributi attuale è "stato corrente della macchina"
(una chiave = un valore, sovrascritto). Per dati come "ID paziente" questo va bene solo se un
device fa un trattamento alla volta e non serve lo storico multi-paziente/multi-trattamento
nella stessa vista — se in futuro serve interrogare "tutti i trattamenti passati di una
macchina" con i loro attributi puntuali, serve una tabella dedicata (es.
`device_records(tenant_id, device_id, record_id, ts, data_json)`), non l'estensione
dell'attuale `device_attributes`. Da valutare quando il caso d'uso concreto lo richiede.

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
- Storico multi-trattamento per dati puntuali (es. più pazienti nel tempo sulla stessa
  macchina) — vedi limite noto in "Upload file client→server + importer per estensione"
  sopra: l'upload file e il meccanismo importer sono già implementati, questo è il pezzo
  rimasto fuori
