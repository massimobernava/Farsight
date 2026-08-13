// Package registry holds the in-memory, MQTT-driven connectivity state
// (online/offline) that farsight-server's control plane renders. There is
// no database for this: retained status messages on the broker are
// themselves the persistence layer, redelivered to us on every
// (re)subscribe. Time-series telemetry doesn't live here at all — that's
// InfluxDB/Grafana's job (see PROJECT_SPEC.md "Componente 2"); this
// package only ever tracks whether a device is currently reachable.
package registry

import (
	"sort"
	"sync"
	"time"
)

// Device is the control plane's view of one machine.
type Device struct {
	TenantID string
	DeviceID string

	// DisplayName/Notes/Attributes are persisted metadata (internal/store),
	// merged in by the caller after List() — the registry itself only
	// tracks live connectivity, never identity metadata or point values.
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
	devices map[string]*Device // key: tenantID + "/" + deviceID
}

func New() *Registry {
	return &Registry{devices: make(map[string]*Device)}
}

func key(tenantID, deviceID string) string {
	return tenantID + "/" + deviceID
}

func (r *Registry) get(tenantID, deviceID string) *Device {
	k := key(tenantID, deviceID)
	d, ok := r.devices[k]
	if !ok {
		d = &Device{TenantID: tenantID, DeviceID: deviceID}
		r.devices[k] = d
	}
	return d
}

// SetStatus updates a device's online/offline state from a StatusTopic message.
func (r *Registry) SetStatus(tenantID, deviceID string, online bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.get(tenantID, deviceID)
	d.Online = online
	d.LastSeen = time.Now()
}

// Touch bumps a device's LastSeen without changing Online — call this on
// any message from the device (telemetry, attributes), not just status
// changes, so LastSeen reflects "last heard from," not just "last time
// online/offline flipped."
func (r *Registry) Touch(tenantID, deviceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.get(tenantID, deviceID).LastSeen = time.Now()
}

// List returns a snapshot of all known devices, sorted by tenant then device ID.
func (r *Registry) List() []Device {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Device, 0, len(r.devices))
	for _, d := range r.devices {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TenantID != out[j].TenantID {
			return out[i].TenantID < out[j].TenantID
		}
		return out[i].DeviceID < out[j].DeviceID
	})
	return out
}
