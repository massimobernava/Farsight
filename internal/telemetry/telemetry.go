// Package telemetry defines the MQTT topic scheme and payload shapes
// shared by farsight-agent (publisher), the C SDK, and farsight-server
// (subscriber / InfluxDB+SQLite bridge). Keeping this in one place is what
// keeps every side in sync as the schema evolves.
//
// Identity on the wire is device_id alone — no tenant_id. A device
// doesn't need to know, or care, which tenant it's grouped under: that's
// purely an access-control/organizational concept farsight-server and
// whoever's looking at the dashboard care about, never something the
// telemetry/VNC/SSH path itself depends on to function. Every device
// lands under the "default" tenant the first time it's ever seen (see
// store.EnsureDevice) and an admin moves it from there — see
// docs/MULTI-TENANCY.md. This used to be tenant_id-in-topic
// (farsight/<tenant>/<device>/...) plus a provisioning-token flow tying
// a device to a tenant at connect time; both are gone now in favor of
// this simpler split, deliberately breaking wire compatibility with any
// device still publishing the old topic shape — there were no production
// deployments worth preserving compatibility for at the time of this
// change.
//
// Three distinct kinds of data travel over MQTT, on separate topics:
//
//   - Telemetry (DataTopic): time-series samples, meant to accumulate in
//     InfluxDB over time (CPU load, sensor readings, anything you'd graph
//     against time). Nothing here is a fixed/required field beyond
//     identity — not even CPU/RAM/disk are privileged; farsight-agent
//     reports them the same way a custom publisher reports anything else.
//     Optionally tagged with RecordID to correlate a burst of samples with
//     a specific session/treatment (see Record below).
//   - Attributes (AttributesTopic): point-in-time facts about a device's
//     CURRENT state — a Tailscale IP, a service's up/down flag, a firmware
//     version — that overwrite rather than accumulate. One value per key
//     per device, always. Wrong tool for anything that repeats over time
//     with each occurrence worth keeping (e.g. "which patient is being
//     treated" — a device treats more than one patient over its life) —
//     that's what Record is for.
//   - Records (RecordsTopic): a self-contained snapshot of point values
//     tied to one occurrence of something (one treatment session, one
//     uploaded file) — accumulates like telemetry, but a JSON object per
//     entry, not a single field. A device's attributes answer "what's true
//     about this device right now"; its records answer "what happened,
//     each time something happened."
package telemetry

import (
	"fmt"
	"strings"
	"time"
)

// StatusTopic is the retained online/offline topic for a device, driven by
// the MQTT connection's Last Will and Testament so the broker itself
// reports a device offline if it drops without a clean disconnect.
func StatusTopic(deviceID string) string {
	return "farsight/" + deviceID + "/status"
}

// DataTopic is the periodic time-series telemetry topic for a device.
func DataTopic(deviceID string) string {
	return "farsight/" + deviceID + "/telemetry"
}

// AttributesTopic is the point-value ("current state") topic for a device.
func AttributesTopic(deviceID string) string {
	return "farsight/" + deviceID + "/attributes"
}

// RecordsTopic is the per-occurrence snapshot topic for a device.
func RecordsTopic(deviceID string) string {
	return "farsight/" + deviceID + "/records"
}

// Subscription wildcards for farsight-server to pick up every device.
const (
	StatusWildcard     = "farsight/+/status"
	DataWildcard       = "farsight/+/telemetry"
	AttributesWildcard = "farsight/+/attributes"
	RecordsWildcard    = "farsight/+/records"
)

// ParseTopic extracts deviceID from a concrete (non-wildcard)
// "farsight/<device>/<kind>" topic, as delivered by the broker for any of
// the wildcard subscriptions above.
func ParseTopic(topic string) (deviceID string, err error) {
	parts := strings.Split(topic, "/")
	if len(parts) != 3 || parts[0] != "farsight" {
		return "", fmt.Errorf("telemetry: unexpected topic shape %q", topic)
	}
	return parts[1], nil
}

// Status values published (retained) on StatusTopic.
const (
	StatusOnline  = "online"
	StatusOffline = "offline"
)

// Payload is the JSON body published on DataTopic — a time-series sample.
// Metrics is a fully open bag: every key becomes its own InfluxDB field,
// no server-side config change needed per new metric name (see
// docs/DEVELOPMENT.md). Key names should be lower_snake_case and
// descriptive — they double as InfluxDB field names.
type Payload struct {
	Timestamp time.Time          `json:"ts"`
	DeviceID  string             `json:"device_id"`
	RecordID  string             `json:"record_id,omitempty"`
	Metrics   map[string]float64 `json:"metrics,omitempty"`
}

// Attribute is the JSON body published on AttributesTopic — one
// point-in-time key/value fact. Value is always a string: SQLite (where
// farsight-server persists these) has no strict column typing anyway, so
// this covers numbers and text uniformly without a second code path.
type Attribute struct {
	Timestamp time.Time `json:"ts"`
	DeviceID  string    `json:"device_id"`
	Key       string    `json:"key"`
	Value     string    `json:"value"`
}

// Record is the JSON body published on RecordsTopic — a snapshot of
// point-in-time facts tied to one occurrence (one treatment session, one
// uploaded file). RecordID identifies that occurrence and is the primary
// key alongside device — publishing the same RecordID again replaces that
// record (idempotent retry), it does not create a second history entry
// the way telemetry would. Data is an open string-keyed bag, same
// reasoning as Attribute.Value: no strict typing, numbers as text are
// fine.
type Record struct {
	Timestamp time.Time         `json:"ts"`
	DeviceID  string            `json:"device_id"`
	RecordID  string            `json:"record_id"`
	Data      map[string]string `json:"data"`
}
