// farsight-server is the control plane: it subscribes to every device's
// MQTT status/telemetry topics and serves a dashboard listing known
// machines with a direct link to each one's noVNC desktop. It never
// proxies VNC/SSH traffic itself — see PROJECT_SPEC.md's core principle.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/farsight/farsight/internal/config"
	"github.com/farsight/farsight/internal/dashboard"
	"github.com/farsight/farsight/internal/registry"
	"github.com/farsight/farsight/internal/store"
	"github.com/farsight/farsight/internal/tailscaleip"
	"github.com/farsight/farsight/internal/telemetry"
)

const defaultConfigPath = "/etc/farsight/server.conf"

func main() {
	if err := run(); err != nil {
		log.Fatalf("farsight-server: %v", err)
	}
}

func run() error {
	confPath := os.Getenv("FARSIGHT_SERVER_CONF")
	if confPath == "" {
		confPath = defaultConfigPath
	}
	cfg, err := config.ParseFile(confPath)
	if err != nil {
		return err
	}

	broker := cfg.Get("MQTT_BROKER", "tcp://127.0.0.1:1883")
	httpPort := cfg.Get("HTTP_PORT", "8080")
	websockifyPort := cfg.GetInt("WEBSOCKIFY_PORT", 6080)
	sshUser := cfg.Get("SSH_USER", "")
	bindIface := cfg.Get("BIND_INTERFACE", "tailscale")
	sqlitePath := cfg.Get("SQLITE_PATH", "/var/lib/farsight/farsight.db")
	uploadDir := cfg.Get("UPLOAD_DIR", "/var/lib/farsight/uploads")
	importersDir := cfg.Get("IMPORTERS_DIR", "/etc/farsight/importers")

	st, err := store.Open(sqlitePath)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bindIP := "127.0.0.1"
	if bindIface == "tailscale" {
		ip, err := tailscaleip.Current(ctx)
		if err != nil {
			return err
		}
		bindIP = ip
	}

	reg := registry.New()

	opts := mqtt.NewClientOptions().
		AddBroker(broker).
		SetClientID("farsight-server").
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetOnConnectHandler(func(c mqtt.Client) {
			log.Printf("connected to broker %s, subscribing", broker)
			c.Subscribe(telemetry.StatusWildcard, 1, onStatus(reg, st))
			c.Subscribe(telemetry.DataWildcard, 1, onData(reg, st))
			c.Subscribe(telemetry.AttributesWildcard, 1, onAttribute(reg, st))
			c.Subscribe(telemetry.RecordsWildcard, 1, onRecord(reg, st))
		})

	client := mqtt.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		return token.Error()
	}
	defer client.Disconnect(250)

	dashCfg := dashboard.Config{WebsockifyPort: websockifyPort, SSHUser: sshUser}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		devices := listWithMeta(reg, st)
		// ?device=<device_id> filters to a single machine — used by the
		// Grafana "Macchina" dashboard's iframe panel, which interpolates
		// its own $device_id variable into this URL. Without the param,
		// every device shows (the "Sistema" dashboard's iframe panel).
		if filter := r.URL.Query().Get("device"); filter != "" {
			filtered := devices[:0]
			for _, d := range devices {
				if d.DeviceID == filter {
					filtered = append(filtered, d)
				}
			}
			devices = filtered
		}
		if err := dashboard.Render(w, devices, dashCfg); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("GET /api/devices", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(listWithMeta(reg, st))
	})
	mux.HandleFunc("POST /devices/{tenant}/{device}/rename", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		tenant, device := r.PathValue("tenant"), r.PathValue("device")
		if err := st.SetDisplayName(tenant, device, r.FormValue("display_name")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	mux.HandleFunc("POST /devices/{tenant}/{device}/upload", uploadHandler(st, uploadDir, importersDir))
	mux.HandleFunc("GET /devices/{tenant}/{device}/files/{filename}", filesHandler(uploadDir))

	addr := bindIP + ":" + httpPort
	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		<-ctx.Done()
		log.Println("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("farsight-server listening on http://%s (Tailscale-only)", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// listWithMeta merges the registry's live connectivity state with each
// device's persisted identity metadata and attributes from SQLite.
func listWithMeta(reg *registry.Registry, st *store.Store) []registry.Device {
	devices := reg.List()
	for i := range devices {
		meta, err := st.GetDevice(devices[i].TenantID, devices[i].DeviceID)
		if err != nil {
			log.Printf("store: device lookup failed for %s/%s: %v", devices[i].TenantID, devices[i].DeviceID, err)
			continue
		}
		devices[i].DisplayName = meta.DisplayName
		devices[i].Notes = meta.Notes

		attrs, err := st.GetAttributes(devices[i].TenantID, devices[i].DeviceID)
		if err != nil {
			log.Printf("store: attributes lookup failed for %s/%s: %v", devices[i].TenantID, devices[i].DeviceID, err)
			continue
		}
		devices[i].Attributes = attrs
	}
	return devices
}

const maxUploadBytes = 50 << 20 // 50MB

// uploadHandler accepts an arbitrary file from a device, stores it under
// uploadDir/<tenant>/<device>/, and — if one is configured — hands it off
// to an external importer. Deliberately format-agnostic: this is the
// generic "client pushes a file to the server" transport from
// PROJECT_SPEC.md's Fase 2 note. farsight-server never parses file
// contents itself; it only knows how to look one script up by extension
// (importersDir/<ext, no dot>) and run it. No importer configured for an
// extension — including proprietary, one-off, "not an official feature"
// formats — means the file is simply saved and nothing else happens.
func uploadHandler(st *store.Store, uploadDir, importersDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant, device := r.PathValue("tenant"), r.PathValue("device")

		// path.Base strips any directory components from the caller-supplied
		// name, so "../../etc/passwd" can't escape the per-device directory.
		filename := path.Base(r.URL.Query().Get("filename"))
		if filename == "" || filename == "." || filename == ".." || filename == "/" {
			http.Error(w, "missing or invalid ?filename=", http.StatusBadRequest)
			return
		}

		dir := filepath.Join(uploadDir, tenant, device)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// O_EXCL makes the name collision-proof outright, rather than just
		// unlikely: two uploads of the same filename landing in the same
		// nanosecond (same device, rapid/looped uploads — e.g. several
		// images for one treatment) would otherwise silently overwrite
		// each other instead of erroring or retrying.
		var f *os.File
		var dest, savedName string
		for attempt := 0; ; attempt++ {
			if attempt > 100 {
				http.Error(w, "could not allocate a unique filename", http.StatusInternalServerError)
				return
			}
			if attempt == 0 {
				savedName = fmt.Sprintf("%d-%s", time.Now().UnixNano(), filename)
			} else {
				savedName = fmt.Sprintf("%d-%d-%s", time.Now().UnixNano(), attempt, filename)
			}
			dest = filepath.Join(dir, savedName)
			var err error
			f, err = os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err == nil {
				break
			}
			if !os.IsExist(err) {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		defer f.Close()

		r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
		n, err := io.Copy(f, r.Body)
		if err != nil {
			os.Remove(dest)
			http.Error(w, "upload too large or connection interrupted", http.StatusBadRequest)
			return
		}

		if err := st.EnsureDevice(tenant, device); err != nil {
			log.Printf("store: ensure device failed for %s/%s: %v", tenant, device, err)
		}

		log.Printf("upload: saved %d bytes to %s", n, dest)
		go runImporter(importersDir, dest, tenant, device)

		// Response body is just the saved filename (no path, no other
		// text) so callers — including the C SDK — can use it as-is, e.g.
		// as the record_id/reference for a farsight_publish_record call
		// linking this specific file to something (a treatment, ...).
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintln(w, savedName)
	}
}

// filesHandler serves back a file previously saved by uploadHandler — the
// read side of the same generic, format-agnostic mechanism. Needed because
// a browser (e.g. an <img> tag in a Grafana panel) can't read the server's
// local filesystem directly; this is the only way anything outside the
// server process gets the bytes back.
func filesHandler(uploadDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant, device := r.PathValue("tenant"), r.PathValue("device")
		filename := path.Base(r.PathValue("filename")) // no path traversal out of the device's own directory
		if filename == "" || filename == "." || filename == ".." || filename == "/" {
			http.Error(w, "invalid filename", http.StatusBadRequest)
			return
		}
		filePath := filepath.Join(uploadDir, tenant, device, filename)

		if ct := mime.TypeByExtension(filepath.Ext(filename)); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		http.ServeFile(w, r, filePath)
	}
}

// runImporter looks up importersDir/<ext> (extension without the leading
// dot, e.g. "dat" for a .dat file) and, if it exists and is executable,
// runs it as: importer <file-path> <tenant> <device>. Whatever the script
// does with that — parse it, push data via MQTT, ignore it — is entirely
// up to whoever configured it; farsight-server just dispatches by
// extension and logs the outcome.
func runImporter(importersDir, filePath, tenant, device string) {
	ext := strings.TrimPrefix(filepath.Ext(filePath), ".")
	if ext == "" {
		return
	}
	importer := filepath.Join(importersDir, ext)
	if info, err := os.Stat(importer); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return // no importer configured for this extension, or not executable
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, importer, filePath, tenant, device)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("importer %s failed for %s: %v\n%s", importer, filePath, err, output)
		return
	}
	log.Printf("importer %s succeeded for %s\n%s", importer, filePath, output)
}

func onStatus(reg *registry.Registry, st *store.Store) mqtt.MessageHandler {
	return func(_ mqtt.Client, msg mqtt.Message) {
		tenantID, deviceID, err := telemetry.ParseTopic(msg.Topic())
		if err != nil {
			log.Printf("status: %v", err)
			return
		}
		online := string(msg.Payload()) == telemetry.StatusOnline
		reg.SetStatus(tenantID, deviceID, online)
		if err := st.EnsureDevice(tenantID, deviceID); err != nil {
			log.Printf("store: ensure device failed for %s/%s: %v", tenantID, deviceID, err)
		}
	}
}

// onData only registers the device's existence — the telemetry itself
// (time-series metrics) is Telegraf's job to write to InfluxDB, not
// farsight-server's; see PROJECT_SPEC.md "Componente 2".
func onData(reg *registry.Registry, st *store.Store) mqtt.MessageHandler {
	return func(_ mqtt.Client, msg mqtt.Message) {
		var p telemetry.Payload
		if err := json.Unmarshal(msg.Payload(), &p); err != nil {
			log.Printf("telemetry: bad payload on %s: %v", msg.Topic(), err)
			return
		}
		reg.Touch(p.TenantID, p.DeviceID)
		if err := st.EnsureDevice(p.TenantID, p.DeviceID); err != nil {
			log.Printf("store: ensure device failed for %s/%s: %v", p.TenantID, p.DeviceID, err)
		}
	}
}

func onAttribute(reg *registry.Registry, st *store.Store) mqtt.MessageHandler {
	return func(_ mqtt.Client, msg mqtt.Message) {
		var a telemetry.Attribute
		if err := json.Unmarshal(msg.Payload(), &a); err != nil {
			log.Printf("attribute: bad payload on %s: %v", msg.Topic(), err)
			return
		}
		reg.Touch(a.TenantID, a.DeviceID)
		if err := st.SetAttribute(a.TenantID, a.DeviceID, a.Key, a.Value); err != nil {
			log.Printf("store: set attribute failed for %s/%s: %v", a.TenantID, a.DeviceID, err)
		}
	}
}

func onRecord(reg *registry.Registry, st *store.Store) mqtt.MessageHandler {
	return func(_ mqtt.Client, msg mqtt.Message) {
		var r telemetry.Record
		if err := json.Unmarshal(msg.Payload(), &r); err != nil {
			log.Printf("record: bad payload on %s: %v", msg.Topic(), err)
			return
		}
		reg.Touch(r.TenantID, r.DeviceID)
		if err := st.SaveRecord(r.TenantID, r.DeviceID, r.RecordID, r.Timestamp, r.Data); err != nil {
			log.Printf("store: save record failed for %s/%s/%s: %v", r.TenantID, r.DeviceID, r.RecordID, err)
		}
	}
}
