// farsight-agent publishes periodic telemetry (Tailscale IP, basic system
// metrics, local service state) to the central MQTT broker, and maintains
// a retained online/offline status topic via MQTT's Last Will mechanism.
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/farsight/farsight/internal/config"
	"github.com/farsight/farsight/internal/svcstate"
	"github.com/farsight/farsight/internal/sysmetrics"
	"github.com/farsight/farsight/internal/tailscaleip"
	"github.com/farsight/farsight/internal/telemetry"
)

const defaultConfigPath = "/etc/farsight/client.conf"

func main() {
	if err := run(); err != nil {
		log.Fatalf("farsight-agent: %v", err)
	}
}

func run() error {
	confPath := os.Getenv("FARSIGHT_CLIENT_CONF")
	if confPath == "" {
		confPath = defaultConfigPath
	}
	cfg, err := config.ParseFile(confPath)
	if err != nil {
		return err
	}

	broker, err := cfg.Require("MQTT_BROKER")
	if err != nil {
		return err
	}
	tenantID, err := cfg.Require("TENANT_ID")
	if err != nil {
		return err
	}
	hostname, _ := os.Hostname()
	deviceID := cfg.Get("DEVICE_ID", hostname)
	interval := time.Duration(cfg.GetInt("PUBLISH_INTERVAL_SECONDS", 30)) * time.Second

	statusTopic := telemetry.StatusTopic(tenantID, deviceID)
	dataTopic := telemetry.DataTopic(tenantID, deviceID)

	opts := mqtt.NewClientOptions().
		AddBroker(broker).
		SetClientID("farsight-agent-"+deviceID).
		SetOrderMatters(false).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetWill(statusTopic, telemetry.StatusOffline, 1, true).
		SetOnConnectHandler(func(c mqtt.Client) {
			log.Printf("connected to broker %s, publishing online status", broker)
			c.Publish(statusTopic, 1, true, telemetry.StatusOnline)
		})

	client := mqtt.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		return token.Error()
	}
	defer func() {
		client.Publish(statusTopic, 1, true, telemetry.StatusOffline).Wait()
		client.Disconnect(250)
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf("farsight-agent started: tenant=%s device=%s interval=%s", tenantID, deviceID, interval)

	// Publish once immediately, then on every tick.
	publishOnce(ctx, client, dataTopic, tenantID, deviceID)
	for {
		select {
		case <-ctx.Done():
			log.Println("shutting down")
			return nil
		case <-ticker.C:
			publishOnce(ctx, client, dataTopic, tenantID, deviceID)
		}
	}
}

func publishOnce(ctx context.Context, client mqtt.Client, topic, tenantID, deviceID string) {
	payload := telemetry.Payload{
		Timestamp: time.Now().UTC(),
		TenantID:  tenantID,
		DeviceID:  deviceID,

		ServiceX11VNCUp:     svcstate.IsActive(ctx, "farsight-x11vnc.service"),
		ServiceWebsockifyUp: svcstate.IsActive(ctx, "farsight-vnc-proxy.service"),
	}

	if ip, err := tailscaleip.Current(ctx); err != nil {
		log.Printf("tailscale ip lookup failed: %v", err)
	} else {
		payload.TailscaleIP = ip
	}

	cpuCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	cpuPct, err := sysmetrics.CPUPercent(cpuCtx, time.Second)
	cancel()
	if err != nil {
		log.Printf("cpu metrics failed: %v", err)
	} else {
		payload.CPUPercent = cpuPct
	}

	if memPct, err := sysmetrics.MemPercent(); err != nil {
		log.Printf("mem metrics failed: %v", err)
	} else {
		payload.MemPercent = memPct
	}

	if diskPct, err := sysmetrics.DiskPercent("/"); err != nil {
		log.Printf("disk metrics failed: %v", err)
	} else {
		payload.DiskPercent = diskPct
	}

	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("marshal telemetry failed: %v", err)
		return
	}

	token := client.Publish(topic, 1, false, body)
	token.Wait()
	if err := token.Error(); err != nil {
		log.Printf("publish failed: %v", err)
	}
}
