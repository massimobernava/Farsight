// Package store persists device identity metadata (display name, notes) —
// the things a user sets through the UI and expects to survive a restart.
// Live MQTT-driven state (online/offline, last metrics) stays in
// internal/registry, which is intentionally just in-memory: that data is
// only ever as fresh as the last message anyway, so there's nothing to
// gain from persisting it, and every device reappears in it automatically
// as soon as it publishes again.
package store

import (
	"database/sql"
	"encoding/json"
	"time"

	_ "modernc.org/sqlite"
)

type DeviceMeta struct {
	TenantID    string
	DeviceID    string
	DisplayName string
	Notes       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}

	const schema = `
	CREATE TABLE IF NOT EXISTS devices (
		tenant_id    TEXT NOT NULL,
		device_id    TEXT NOT NULL,
		display_name TEXT NOT NULL DEFAULT '',
		notes        TEXT NOT NULL DEFAULT '',
		created_at   TEXT NOT NULL,
		updated_at   TEXT NOT NULL,
		PRIMARY KEY (tenant_id, device_id)
	);
	CREATE TABLE IF NOT EXISTS device_attributes (
		tenant_id  TEXT NOT NULL,
		device_id  TEXT NOT NULL,
		key        TEXT NOT NULL,
		value      TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (tenant_id, device_id, key)
	);
	CREATE TABLE IF NOT EXISTS device_records (
		tenant_id  TEXT NOT NULL,
		device_id  TEXT NOT NULL,
		record_id  TEXT NOT NULL,
		ts         TEXT NOT NULL,
		data_json  TEXT NOT NULL,
		PRIMARY KEY (tenant_id, device_id, record_id)
	);`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// EnsureDevice inserts a row for (tenantID, deviceID) if one doesn't exist
// yet, so every device that has ever published shows up in the registry —
// including in Grafana's own SQLite datasource — even before anyone sets a
// display name for it.
func (s *Store) EnsureDevice(tenantID, deviceID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(`
		INSERT INTO devices (tenant_id, device_id, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (tenant_id, device_id) DO NOTHING`,
		tenantID, deviceID, now, now)
	return err
}

func (s *Store) SetDisplayName(tenantID, deviceID, displayName string) error {
	if err := s.EnsureDevice(tenantID, deviceID); err != nil {
		return err
	}
	_, err := s.db.Exec(`
		UPDATE devices SET display_name = ?, updated_at = ?
		WHERE tenant_id = ? AND device_id = ?`,
		displayName, time.Now().UTC().Format(time.RFC3339), tenantID, deviceID)
	return err
}

func (s *Store) SetNotes(tenantID, deviceID, notes string) error {
	if err := s.EnsureDevice(tenantID, deviceID); err != nil {
		return err
	}
	_, err := s.db.Exec(`
		UPDATE devices SET notes = ?, updated_at = ?
		WHERE tenant_id = ? AND device_id = ?`,
		notes, time.Now().UTC().Format(time.RFC3339), tenantID, deviceID)
	return err
}

// SetAttribute upserts one point-in-time key/value fact for a device —
// overwrites the previous value for that key, never accumulates. value is
// always stored as text; SQLite doesn't enforce column typing, so this
// covers numeric and string attributes uniformly.
func (s *Store) SetAttribute(tenantID, deviceID, key, value string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(`
		INSERT INTO device_attributes (tenant_id, device_id, key, value, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id, device_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		tenantID, deviceID, key, value, now)
	return err
}

// GetAttributes returns every known key/value attribute for a device.
func (s *Store) GetAttributes(tenantID, deviceID string) (map[string]string, error) {
	rows, err := s.db.Query(`
		SELECT key, value FROM device_attributes WHERE tenant_id = ? AND device_id = ?`,
		tenantID, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SaveRecord upserts a full snapshot tied to one occurrence (recordID) —
// unlike SetAttribute, this accumulates one row per distinct recordID
// instead of overwriting a single current value; publishing the same
// recordID again replaces just that occurrence (idempotent retry), not
// history. data is stored as a JSON object (SQLite has no strict column
// typing, and Grafana's SQLite plugin can query it with json_extract()).
func (s *Store) SaveRecord(tenantID, deviceID, recordID string, ts time.Time, data map[string]string) error {
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO device_records (tenant_id, device_id, record_id, ts, data_json)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id, device_id, record_id) DO UPDATE SET ts = excluded.ts, data_json = excluded.data_json`,
		tenantID, deviceID, recordID, ts.UTC().Format(time.RFC3339), string(dataJSON))
	return err
}

// GetDevice returns the persisted metadata for (tenantID, deviceID), or a
// zero-value DeviceMeta (no error) if it isn't known yet — callers merge
// this with live registry state, and a device can be live before it has
// ever been persisted.
func (s *Store) GetDevice(tenantID, deviceID string) (DeviceMeta, error) {
	var m DeviceMeta
	var createdAt, updatedAt string
	row := s.db.QueryRow(`
		SELECT tenant_id, device_id, display_name, notes, created_at, updated_at
		FROM devices WHERE tenant_id = ? AND device_id = ?`, tenantID, deviceID)
	err := row.Scan(&m.TenantID, &m.DeviceID, &m.DisplayName, &m.Notes, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		m.TenantID, m.DeviceID = tenantID, deviceID
		return m, nil
	}
	if err != nil {
		return DeviceMeta{}, err
	}
	m.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	m.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return m, nil
}
