// Package registry holds the in-memory, MQTT-driven connectivity state
// (online/offline) that farsight-server's control plane renders. There is
// no database for this: retained status messages on the broker are
// themselves the persistence layer, redelivered to us on every
// (re)subscribe. Time-series telemetry doesn't live here at all — that's
// InfluxDB/Grafana's job; this package only ever tracks whether a device
// is currently reachable.
//
// Keyed by device_id alone — the wire protocol carries no tenant_id (see
// internal/telemetry), so the registry never sees one either. TenantID on
// Device is populated later, by the caller merging in store.GetDevice's
// current assignment (see cmd/farsight-server's listWithMeta) — same
// pattern already used for DisplayName/Notes/Attributes below.
package registry

import (
	"sort"
	"sync"
	"time"
)

// Device is the control plane's view of one machine.
type Device struct {
	DeviceID string

	// TenantID/DisplayName/Notes/Attributes are persisted metadata
	// (internal/store), merged in by the caller after List() — the
	// registry itself only tracks live connectivity, never identity
	// metadata or point values.
	TenantID    string
	DisplayName string
	Notes       string
	Attributes  map[string]string

	Online   bool
	LastSeen time.Time
}

// Registry is safe for concurrent use by the MQTT subscriber goroutine and
// HTTP handler goroutines.
type Registry struct {
	mu      sync.RWMutex
	devices map[string]*Device // key: deviceID
}

func New() *Registry {
	return &Registry{devices: make(map[string]*Device)}
}

func (r *Registry) get(deviceID string) *Device {
	d, ok := r.devices[deviceID]
	if !ok {
		d = &Device{DeviceID: deviceID}
		r.devices[deviceID] = d
	}
	return d
}

// SetStatus updates a device's online/offline state from a StatusTopic message.
func (r *Registry) SetStatus(deviceID string, online bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.get(deviceID)
	d.Online = online
	d.LastSeen = time.Now()
}

// Touch bumps a device's LastSeen without changing Online — call this on
// any message from the device (telemetry, attributes), not just status
// changes, so LastSeen reflects "last heard from," not just "last time
// online/offline flipped."
func (r *Registry) Touch(deviceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.get(deviceID).LastSeen = time.Now()
}

// List returns a snapshot of all known devices, sorted by device ID.
func (r *Registry) List() []Device {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Device, 0, len(r.devices))
	for _, d := range r.devices {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].DeviceID < out[j].DeviceID
	})
	return out
}
