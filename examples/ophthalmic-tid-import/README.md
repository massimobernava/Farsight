# Esempio: importer per file .dat proprietari

**Non è una feature ufficiale di Farsight.** Dimostra che il sistema esistente (upload
generico + MQTT) può assorbire un formato dati proprietario/interno — in questo caso i file
`.dat` prodotti da un dispositivo oftalmico (vedi
[`data/TID-000001-000276-111125.dat`](../../data/TID-000001-000276-111125.dat) nel repo per un
file reale) — senza che il formato specifico debba entrare nel codice base di
`farsight-server`.

## Come funziona

1. Il client manda il file al server con l'SDK ufficiale:
   `farsight_upload_file(client, 0, "TID.dat")` — vedi [`sdk/c/farsight.h`](../../sdk/c/farsight.h).
2. `farsight-server` salva il file, poi cerca uno script eseguibile chiamato come
   l'estensione del file dentro `IMPORTERS_DIR` (default `/etc/farsight/importers/`, vedi
   `server.conf`) — per un file `.dat`, cerca `/etc/farsight/importers/dat`. Se non esiste,
   il file resta semplicemente salvato, nessun errore: il core non sa nulla del formato.
3. Se lo script esiste, il server lo lancia come:
   `<script> <percorso-file> <tenant_id> <device_id>` — [`import_tid.py`](import_tid.py) fa
   esattamente questo: parsa il formato (header chiave:valore, blocco `[SENSOR]`, tabella TSV),
   e pubblica su MQTT usando **gli stessi canali che usa qualunque publisher** (vedi
   [docs/DEVELOPMENT.md](../../docs/DEVELOPMENT.md)):
   - l'header (ID paziente, parametri intervento, ID trattamento, punteggi) → **un record**
     (topic `records`, `record_id` = `TREATMENT ID` dal file) → SQLite, **accumula** — un
     dispositivo tratta più pazienti nella sua vita, un attributo sovrascriverebbe il paziente
     precedente, un record no
   - la tabella (colonna `TIME (s)` come asse) → topic `telemetry`, **taggata con lo stesso
     `record_id`** → InfluxDB, storico interrogabile/graficabile in Grafana filtrato per singolo
     trattamento

Nessuna riga di questo formato specifico vive in `cmd/farsight-server` o `internal/`.

## Vederlo in Grafana

La dashboard **"Farsight - Trattamenti"** (creata manualmente per questa demo, non provisionata
dal pacchetto — vedi nota in `docs/DEVELOPMENT.md`) ha un menu a tendina popolato dai
`record_id`/pazienti noti (query SQLite `SELECT record_id AS __value, patient_id || ... AS
__text FROM device_records`), con un pannello di dettaglio (paziente, punteggi, parametri
intervento — letti da SQLite via `json_extract`) e grafici InfluxDB filtrati sullo stesso
trattamento selezionato.

## Provarlo

```bash
# 1. installa lo script come importer per estensione .dat
sudo mkdir -p /etc/farsight/importers
sudo cp import_tid.py /etc/farsight/importers/dat
sudo chmod +x /etc/farsight/importers/dat
sudo apt-get install python3-paho-mqtt

# 2. manda il file con l'SDK ufficiale (farsight_upload_file, vedi sdk/c/README.md)
#    o, per un test rapido senza scrivere C, direttamente da CLI:

```bash
curl -X POST --data-binary @../../data/TID-000001-000276-111125.dat \
  "http://<ip-tailscale-server>:8080/devices/default/153210001/upload?filename=TID.dat"
```

Il device `153210001` (il `DEVICE S/N` nel file) comparirà in dashboard/Grafana; il trattamento
`TID-000001-000276` comparirà nel menu a tendina di "Farsight - Trattamenti".

## Limiti noti (è una demo, non un prodotto)

- `TIME (s)` nel file è secondi trascorsi dall'inizio del trattamento, non un orario reale —
  il timestamp ricostruito ancora l'ultima riga a "adesso" e conta all'indietro. Un'integrazione
  vera avrebbe bisogno dell'orario reale di inizio, non presente in questo formato.
- La colonna `CBIT` (stringa di byte) non viene mappata — non è una metrica scalare.
- `record_id` (dal `TREATMENT ID` del file) è la chiave che lega record e serie temporale — se
  due dispositivi diversi producessero per errore lo stesso `TREATMENT ID` si mescolerebbero
  nel filtro Grafana. Non validato/reso univoco oltre a fidarsi del dato del dispositivo.
