// Package telemetry defines the MQTT topic scheme and payload shape shared
// by farsight-agent (publisher) and, later, farsight-server (subscriber /
// InfluxDB bridge). Keeping this in one place is what keeps the two sides
// in sync as the schema evolves.
package telemetry

import (
	"fmt"
	"strings"
	"time"
)

// StatusTopic is the retained online/offline topic for a device, driven by
// the MQTT connection's Last Will and Testament so the broker itself
// reports a device offline if it drops without a clean disconnect.
func StatusTopic(tenantID, deviceID string) string {
	return "farsight/" + tenantID + "/" + deviceID + "/status"
}

// DataTopic is the periodic telemetry topic for a device.
func DataTopic(tenantID, deviceID string) string {
	return "farsight/" + tenantID + "/" + deviceID + "/telemetry"
}

// Subscription wildcards for farsight-server to pick up every device.
const (
	StatusWildcard = "farsight/+/+/status"
	DataWildcard   = "farsight/+/+/telemetry"
)

// ParseTopic extracts tenantID and deviceID from a concrete (non-wildcard)
// "farsight/<tenant>/<device>/<kind>" topic, as delivered by the broker for
// a StatusWildcard/DataWildcard subscription.
func ParseTopic(topic string) (tenantID, deviceID string, err error) {
	parts := strings.Split(topic, "/")
	if len(parts) != 4 || parts[0] != "farsight" {
		return "", "", fmt.Errorf("telemetry: unexpected topic shape %q", topic)
	}
	return parts[1], parts[2], nil
}

// Status values published (retained) on StatusTopic.
const (
	StatusOnline  = "online"
	StatusOffline = "offline"
)

// Payload is the JSON body published periodically on DataTopic.
//
// Field names double as the future InfluxDB tag/field names (see
// PROJECT_SPEC.md "Fase 2"): keep them stable, lower_snake_case, and
// descriptive enough that a natural-language query over the telemetry
// makes sense without a lookup table.
type Payload struct {
	Timestamp   time.Time `json:"ts"`
	TenantID    string    `json:"tenant_id"`
	DeviceID    string    `json:"device_id"`
	TailscaleIP string    `json:"tailscale_ip,omitempty"`

	CPUPercent  float64 `json:"cpu_percent"`
	MemPercent  float64 `json:"mem_percent"`
	DiskPercent float64 `json:"disk_percent"`

	ServiceX11VNCUp     bool `json:"service_x11vnc_up"`
	ServiceWebsockifyUp bool `json:"service_websockify_up"`

	// Metrics is an open bag for anything application/device-specific that
	// isn't one of the fixed fields above — patient counts, sensor
	// readings, whatever the machine running farsight-agent (or a custom
	// publisher, see docs/DEVELOPMENT.md) wants to report. Each key becomes
	// its own InfluxDB field with no server-side config change (Telegraf's
	// json_v2 "object" block auto-discovers them) — so the sender picks
	// the key names, and they'd better be clear ones, since nothing here
	// validates or namespaces them.
	Metrics map[string]float64 `json:"metrics,omitempty"`
}
