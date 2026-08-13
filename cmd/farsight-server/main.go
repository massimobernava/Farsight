// farsight-server is the control plane: it subscribes to every device's
// MQTT status/telemetry topics and serves a dashboard listing known
// machines with a direct link to each one's noVNC desktop. It never
// proxies VNC/SSH traffic itself — see PROJECT_SPEC.md's core principle.
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
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
