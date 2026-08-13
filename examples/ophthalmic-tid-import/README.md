# Esempio: importer per file .dat proprietari

**Non è una feature ufficiale di Farsight.** Dimostra che il sistema esistente (upload
generico + MQTT) può assorbire un formato dati proprietario/interno — in questo caso i file
`.dat` prodotti da un dispositivo oftalmico (vedi
[`data/TID-000001-000276-111125.dat`](../../data/TID-000001-000276-111125.dat) nel repo per un
file reale) — senza che il formato specifico debba entrare nel codice base di
`farsight-server`.

## Come funziona

1. Il client manda il file al server: `POST /devices/<tenant>/<device>/upload?filename=...`
   ([`upload_file.c`](upload_file.c), demo con libcurl — non fa parte dell'SDK ufficiale in
   `sdk/c/`, che non sa nulla di trasferimento file).
2. `farsight-server` salva il file, poi cerca uno script eseguibile chiamato come
   l'estensione del file dentro `IMPORTERS_DIR` (default `/etc/farsight/importers/`, vedi
   `server.conf`) — per un file `.dat`, cerca `/etc/farsight/importers/dat`. Se non esiste,
   il file resta semplicemente salvato, nessun errore: il core non sa nulla del formato.
3. Se lo script esiste, il server lo lancia come:
   `<script> <percorso-file> <tenant_id> <device_id>` — [`import_tid.py`](import_tid.py) fa
   esattamente questo: parsa il formato (header chiave:valore, blocco `[SENSOR]`, tabella TSV),
   e pubblica su MQTT usando **gli stessi due canali che usa qualunque publisher** (vedi
   [docs/DEVELOPMENT.md](../../docs/DEVELOPMENT.md)):
   - valori puntuali (ID paziente, parametri intervento, ID trattamento, punteggi) → topic
     `attributes` → SQLite
   - la tabella (colonna `TIME (s)` come asse) → topic `telemetry` → InfluxDB, storico
     interrogabile/graficabile in Grafana

Nessuna riga di questo formato specifico vive in `cmd/farsight-server` o `internal/`.

## Provarlo

```bash
# 1. installa lo script come importer per estensione .dat
sudo mkdir -p /etc/farsight/importers
sudo cp import_tid.py /etc/farsight/importers/dat
sudo chmod +x /etc/farsight/importers/dat
sudo apt-get install python3-paho-mqtt

# 2. manda un file (da CLI, o con upload_file.c)
curl -X POST --data-binary @../../data/TID-000001-000276-111125.dat \
  "http://<ip-tailscale-server>:8080/devices/default/153210001/upload?filename=TID.dat"

# oppure, da C:
cc upload_file.c -o farsight-upload -lcurl
./farsight-upload http://<ip-tailscale-server>:8080 default 153210001 ../../data/TID-000001-000276-111125.dat
```

Il device `153210001` (il `DEVICE S/N` nel file) comparirà in dashboard/Grafana con gli
attributi (paziente, punteggi, ecc.) e la serie temporale del trattamento.

## Limiti noti (è una demo, non un prodotto)

- `TIME (s)` nel file è secondi trascorsi dall'inizio del trattamento, non un orario reale —
  il timestamp ricostruito ancora l'ultima riga a "adesso" e conta all'indietro. Un'integrazione
  vera avrebbe bisogno dell'orario reale di inizio, non presente in questo formato.
- La colonna `CBIT` (stringa di byte) non viene mappata — non è una metrica scalare.
- Più trattamenti sulla stessa macchina sovrascrivono gli attributi puntuali (es. `patient_id`
  del trattamento precedente si perde) — per lo storico multi-trattamento servirebbe una tabella
  dedicata, non il meccanismo attributi attuale (pensato per "stato corrente della macchina",
  non "storico dei trattamenti"). Fuori scope per questa demo.
