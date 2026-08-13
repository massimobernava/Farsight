// Package registry holds the in-memory, MQTT-driven view of known devices
// that farsight-server's control plane renders. There is no database in
// the prototype: retained status messages on the broker are themselves the
// persistence layer, redelivered to us on every (re)subscribe.
package registry

import (
	"sort"
	"sync"
	"time"

	"github.com/farsight/farsight/internal/telemetry"
)

// Device is the control plane's view of one machine.
type Device struct {
	TenantID string
	DeviceID string

	// DisplayName/Notes are persisted metadata (internal/store), merged in
	// by the caller after List() — the registry itself only tracks live
	// MQTT state, never identity metadata.
	DisplayName string
	Notes       string

	Online   bool
	LastSeen time.Time

	TailscaleIP         string
	CPUPercent          float64
	MemPercent          float64
	DiskPercent         float64
	ServiceX11VNCUp     bool
	ServiceWebsockifyUp bool
	LastTelemetryAt     time.Time
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

// SetTelemetry updates a device's last-known metrics from a DataTopic message.
func (r *Registry) SetTelemetry(p telemetry.Payload) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.get(p.TenantID, p.DeviceID)
	d.TailscaleIP = p.TailscaleIP
	d.CPUPercent = p.CPUPercent
	d.MemPercent = p.MemPercent
	d.DiskPercent = p.DiskPercent
	d.ServiceX11VNCUp = p.ServiceX11VNCUp
	d.ServiceWebsockifyUp = p.ServiceWebsockifyUp
	d.LastTelemetryAt = p.Timestamp
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
