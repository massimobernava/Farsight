// farsight-agent publishes point-in-time attributes (Tailscale IP, remote
// desktop availability) and, if enabled, periodic time-series telemetry
// (CPU/RAM/disk) to the central MQTT broker. It also maintains a retained
// online/offline status topic via MQTT's Last Will mechanism — the broker
// itself marks the device offline the moment the connection drops, no
// periodic re-announcement needed.
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
	// Telemetry (CPU/RAM/disk as a time series) is opt-in, not a "standard"
	// feature every publisher is forced into — see PROJECT_SPEC.md. It
	// defaults to on for our own agent since that's genuinely useful out
	// of the box, but PUBLISH_TELEMETRY=false turns it off.
	publishTelemetry := cfg.Get("PUBLISH_TELEMETRY", "true") != "false"

	statusTopic := telemetry.StatusTopic(tenantID, deviceID)
	dataTopic := telemetry.DataTopic(tenantID, deviceID)
	attributesTopic := telemetry.AttributesTopic(tenantID, deviceID)

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

	log.Printf("farsight-agent started: tenant=%s device=%s interval=%s telemetry=%v",
		tenantID, deviceID, interval, publishTelemetry)

	publish := func() {
		publishAttributes(ctx, client, attributesTopic, tenantID, deviceID)
		if publishTelemetry {
			publishMetrics(ctx, client, dataTopic, tenantID, deviceID)
		}
	}

	publish()
	for {
		select {
		case <-ctx.Done():
			log.Println("shutting down")
			return nil
		case <-ticker.C:
			publish()
		}
	}
}

// publishAttributes reports point-in-time facts about this machine's
// current state — never a growing time series, just "here's what's true
// right now." Deliberately kept to what's actually useful for someone
// deciding whether/how to connect: no separate x11vnc/websockify booleans,
// just whether the desktop is reachable.
func publishAttributes(ctx context.Context, client mqtt.Client, topic, tenantID, deviceID string) {
	if ip, err := tailscaleip.Current(ctx); err != nil {
		log.Printf("tailscale ip lookup failed: %v", err)
	} else {
		publishAttribute(client, topic, tenantID, deviceID, "tailscale_ip", ip)
	}

	x11vncUp := svcstate.IsActive(ctx, "farsight-x11vnc.service")
	websockifyUp := svcstate.IsActive(ctx, "farsight-vnc-proxy.service")
	desktopAvailable := "false"
	if x11vncUp && websockifyUp {
		desktopAvailable = "true"
	}
	publishAttribute(client, topic, tenantID, deviceID, "desktop_available", desktopAvailable)
}

func publishAttribute(client mqtt.Client, topic, tenantID, deviceID, key, value string) {
	body, err := json.Marshal(telemetry.Attribute{
		Timestamp: time.Now().UTC(),
		TenantID:  tenantID,
		DeviceID:  deviceID,
		Key:       key,
		Value:     value,
	})
	if err != nil {
		log.Printf("marshal attribute %q failed: %v", key, err)
		return
	}
	token := client.Publish(topic, 1, false, body)
	token.Wait()
	if err := token.Error(); err != nil {
		log.Printf("publish attribute %q failed: %v", key, err)
	}
}

func publishMetrics(ctx context.Context, client mqtt.Client, topic, tenantID, deviceID string) {
	metrics := map[string]float64{}

	cpuCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	cpuPct, err := sysmetrics.CPUPercent(cpuCtx, time.Second)
	cancel()
	if err != nil {
		log.Printf("cpu metrics failed: %v", err)
	} else {
		metrics["cpu_percent"] = cpuPct
	}

	if memPct, err := sysmetrics.MemPercent(); err != nil {
		log.Printf("mem metrics failed: %v", err)
	} else {
		metrics["mem_percent"] = memPct
	}

	if diskPct, err := sysmetrics.DiskPercent("/"); err != nil {
		log.Printf("disk metrics failed: %v", err)
	} else {
		metrics["disk_percent"] = diskPct
	}

	body, err := json.Marshal(telemetry.Payload{
		Timestamp: time.Now().UTC(),
		TenantID:  tenantID,
		DeviceID:  deviceID,
		Metrics:   metrics,
	})
	if err != nil {
		log.Printf("marshal telemetry failed: %v", err)
		return
	}

	token := client.Publish(topic, 1, false, body)
	token.Wait()
	if err := token.Error(); err != nil {
		log.Printf("publish telemetry failed: %v", err)
	}
}
