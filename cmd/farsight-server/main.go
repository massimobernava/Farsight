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
			c.Subscribe(telemetry.StatusWildcard, 1, onStatus(reg))
			c.Subscribe(telemetry.DataWildcard, 1, onData(reg))
		})

	client := mqtt.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		return token.Error()
	}
	defer client.Disconnect(250)

	dashCfg := dashboard.Config{WebsockifyPort: websockifyPort, SSHUser: sshUser}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := dashboard.Render(w, reg.List(), dashCfg); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("/api/devices", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(reg.List())
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

func onStatus(reg *registry.Registry) mqtt.MessageHandler {
	return func(_ mqtt.Client, msg mqtt.Message) {
		tenantID, deviceID, err := telemetry.ParseTopic(msg.Topic())
		if err != nil {
			log.Printf("status: %v", err)
			return
		}
		online := string(msg.Payload()) == telemetry.StatusOnline
		reg.SetStatus(tenantID, deviceID, online)
	}
}

func onData(reg *registry.Registry) mqtt.MessageHandler {
	return func(_ mqtt.Client, msg mqtt.Message) {
		var p telemetry.Payload
		if err := json.Unmarshal(msg.Payload(), &p); err != nil {
			log.Printf("telemetry: bad payload on %s: %v", msg.Topic(), err)
			return
		}
		reg.SetTelemetry(p)
	}
}
