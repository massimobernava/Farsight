// Package store persists device identity metadata (display name, notes,
// current tenant) — the things a user sets through the UI and expects to
// survive a restart. Live MQTT-driven state (online/offline, last
// metrics) stays in internal/registry, which is intentionally just
// in-memory: that data is only ever as fresh as the last message anyway,
// so there's nothing to gain from persisting it, and every device
// reappears in it automatically as soon as it publishes again.
//
// device_id is the sole identity a device carries on the wire (see
// internal/telemetry) — tenant_id lives only here, as a plain mutable
// column on devices, assigned by an admin (default: "default" for any
// device nobody has moved yet). This is a deliberate simplification: a
// device doesn't need to know or care which tenant it's grouped under to
// publish telemetry or serve VNC/SSH, so it never carries one — see
// docs/MULTI-TENANCY.md. Earlier versions of this schema keyed devices by
// (tenant_id, device_id) and required a client-side provisioning token
// tying a device to its tenant at connect time; both are gone now,
// deliberately breaking compatibility with a database from before this
// change — there was no production data worth preserving migration logic
// for at the time.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// DefaultTenantID is where every device lands the first time it's ever
// seen — nobody has to pre-register or provision a tenant before a device
// can start publishing. An admin moves it from here via ReassignDevice
// once it's clear which tenant it actually belongs to.
const DefaultTenantID = "default"

// recordTopLevelColumns are device_records' real SQL columns — anything
// else in a filter is assumed to be a key inside data_json instead. Kept
// as a fixed, small set on purpose: it's the only place that needs to
// know the table's actual shape, everything data-side stays schema-less.
// No tenant_id here: records aren't tenant-scoped in storage at all (see
// package doc) — a device's current tenant is a devices-table lookup, not
// a per-record fact.
var recordTopLevelColumns = map[string]bool{
	"device_id": true, "record_id": true, "ts": true,
}

type DeviceMeta struct {
	DeviceID    string
	TenantID    string
	DisplayName string
	Notes       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	// Two real, external sources of concurrent access to this same file:
	// farsight-server's own goroutines (MQTT-driven writes racing HTTP-driven
	// reads/writes) and Grafana's SQLite datasource plugin, which reads this
	// file directly, in its own process, entirely outside our control.
	// SQLite's default rollback-journal mode blocks a writer against any
	// concurrent reader and vice versa, with no wait — a losing side gets
	// "database is locked (5) (SQLITE_BUSY)" immediately (observed for real
	// on zeus: intermittent EnsureDevice/IsAdmin failures, hours apart).
	// WAL mode lets readers and one writer proceed without blocking each
	// other; busy_timeout covers the remaining writer-vs-writer case by
	// waiting briefly and retrying at the SQLite level instead of erroring
	// out on the first contended millisecond.
	//
	// Both are passed as DSN _pragma params (modernc.org/sqlite-specific),
	// not as a PRAGMA db.Exec() after opening — database/sql pools multiple
	// underlying connections, and a PRAGMA executed once only takes effect
	// on whichever single connection happened to run it. journal_mode=WAL
	// is persisted in the database file itself so that part would've stuck
	// regardless, but busy_timeout is a per-connection runtime setting: any
	// later connection the pool opens fresh would silently get the SQLite
	// default (0 — fail immediately) instead. Caught by
	// TestConcurrentWrites actually deadlocking under load with the
	// db.Exec() version of this fix, before it ever reached zeus.
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}

	const schema = `
	CREATE TABLE IF NOT EXISTS devices (
		device_id    TEXT PRIMARY KEY,
		tenant_id    TEXT NOT NULL DEFAULT 'default',
		display_name TEXT NOT NULL DEFAULT '',
		notes        TEXT NOT NULL DEFAULT '',
		created_at   TEXT NOT NULL,
		updated_at   TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS device_attributes (
		device_id  TEXT NOT NULL,
		key        TEXT NOT NULL,
		value      TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (device_id, key)
	);
	CREATE TABLE IF NOT EXISTS device_records (
		device_id  TEXT NOT NULL,
		record_id  TEXT NOT NULL,
		ts         TEXT NOT NULL,
		data_json  TEXT NOT NULL,
		PRIMARY KEY (device_id, record_id)
	);
	CREATE TABLE IF NOT EXISTS tenants (
		tenant_id    TEXT PRIMARY KEY,
		display_name TEXT NOT NULL DEFAULT '',
		created_at   TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS tenant_members (
		tenant_id  TEXT NOT NULL,
		login      TEXT NOT NULL,
		role       TEXT NOT NULL DEFAULT 'viewer',
		created_at TEXT NOT NULL,
		PRIMARY KEY (tenant_id, login)
	);
	CREATE TABLE IF NOT EXISTS tenant_grafana_orgs (
		tenant_id  TEXT PRIMARY KEY,
		org_id     INTEGER NOT NULL,
		created_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS farsight_admins (
		login      TEXT PRIMARY KEY,
		created_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS llm_conversations (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		tenant_id  TEXT NOT NULL,
		login      TEXT NOT NULL,
		title      TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS llm_messages (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		conversation_id INTEGER NOT NULL,
		role            TEXT NOT NULL,
		content         TEXT NOT NULL DEFAULT '',
		tool_name       TEXT NOT NULL DEFAULT '',
		tool_args       TEXT NOT NULL DEFAULT '',
		tool_result     TEXT NOT NULL DEFAULT '',
		created_at      TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS llm_tool_calls (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		tenant_id  TEXT NOT NULL,
		login      TEXT NOT NULL,
		tool_name  TEXT NOT NULL,
		created_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS settings (
		key        TEXT PRIMARY KEY,
		value      TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}

	// A database from before device_id became the sole identity key (see
	// package doc) has a "devices" table with a different shape — a
	// composite (tenant_id, device_id) primary key instead of device_id
	// alone. CREATE TABLE IF NOT EXISTS above silently no-ops against
	// that, which would otherwise fail confusingly deep inside a query
	// later. Detect it here and fail loudly with an actionable message
	// instead: this is a deliberate breaking schema change, not something
	// worth writing migration logic for.
	pkCols, err := primaryKeyColumns(db, "devices")
	if err != nil {
		db.Close()
		return nil, err
	}
	if len(pkCols) != 1 || pkCols[0] != "device_id" {
		db.Close()
		return nil, fmt.Errorf(
			"store: %s has an old devices table schema (primary key %v, expected just [device_id]) — "+
				"this Farsight version changed how devices are identified (see docs/MULTI-TENANCY.md); "+
				"move the old file aside and let farsight-server create a fresh one", path, pkCols)
	}

	// Migration: tenant_members predates the role column (Tappa 4) —
	// CREATE TABLE IF NOT EXISTS above is a no-op on an install that
	// already has the table, so a plain ALTER TABLE is needed to add it
	// to existing databases. SQLite has no "ADD COLUMN IF NOT EXISTS",
	// hence the pragma check first.
	hasRole, err := hasColumn(db, "tenant_members", "role")
	if err != nil {
		db.Close()
		return nil, err
	}
	if !hasRole {
		if _, err := db.Exec(`ALTER TABLE tenant_members ADD COLUMN role TEXT NOT NULL DEFAULT 'viewer'`); err != nil {
			db.Close()
			return nil, err
		}
	}

	// provisioning_tokens is gone (see package doc) — drop it if an older
	// database still has it. Harmless either way, just tidy.
	if _, err := db.Exec(`DROP TABLE IF EXISTS provisioning_tokens`); err != nil {
		db.Close()
		return nil, err
	}

	// Bootstrap: DefaultTenantID must always exist so a device that's
	// never been touched by an admin (EnsureDevice defaults it there) and
	// ReassignDevice's "does the target tenant exist" check both have
	// somewhere real to point at, with zero setup required.
	if _, err := db.Exec(`
		INSERT INTO tenants (tenant_id, display_name, created_at)
		VALUES (?, ?, ?)
		ON CONFLICT (tenant_id) DO NOTHING`,
		DefaultTenantID, "Default", time.Now().UTC().Format(time.RFC3339)); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

// BootstrapAdmins seeds farsight_admins from logins (server.conf's
// ADMIN_TAILSCALE_LOGINS) the first time only — if the table is already
// non-empty, this is a no-op. Breaks the chicken-and-egg problem of the
// admin panel needing an admin to open it: after the first boot, admin
// status lives entirely in the database (see AddAdmin/RemoveAdmin), edited
// live from the UI, no restart needed — the conf value only matters once.
func (s *Store) BootstrapAdmins(logins []string) error {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM farsight_admins`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	for _, login := range logins {
		if err := s.AddAdmin(login); err != nil {
			return err
		}
	}
	return nil
}

// IsAdmin reports whether login is a Farsight admin — checked on every
// request (see cmd/farsight-server's identify), not cached at boot, so a
// change made via the admin panel takes effect immediately.
func (s *Store) IsAdmin(login string) (bool, error) {
	var exists int
	err := s.db.QueryRow(`SELECT 1 FROM farsight_admins WHERE login = ?`, login).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ListAdmins returns every Farsight admin login, alphabetically.
func (s *Store) ListAdmins() ([]string, error) {
	rows, err := s.db.Query(`SELECT login FROM farsight_admins ORDER BY login`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var login string
		if err := rows.Scan(&login); err != nil {
			return nil, err
		}
		out = append(out, login)
	}
	return out, rows.Err()
}

func (s *Store) AddAdmin(login string) error {
	_, err := s.db.Exec(`
		INSERT INTO farsight_admins (login, created_at)
		VALUES (?, ?)
		ON CONFLICT (login) DO NOTHING`,
		login, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Store) RemoveAdmin(login string) error {
	_, err := s.db.Exec(`DELETE FROM farsight_admins WHERE login = ?`, login)
	return err
}

func primaryKeyColumns(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		if pk > 0 {
			cols = append(cols, name)
		}
	}
	return cols, rows.Err()
}

func hasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) Close() error {
	return s.db.Close()
}

// EnsureDevice inserts a row for deviceID if one doesn't exist yet, so
// every device that has ever published shows up in the registry —
// including in Grafana's own SQLite datasource — even before anyone sets
// a display name for it. A brand new device lands in DefaultTenantID; an
// admin moves it from there with ReassignDevice.
func (s *Store) EnsureDevice(deviceID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(`
		INSERT INTO devices (device_id, tenant_id, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (device_id) DO NOTHING`,
		deviceID, DefaultTenantID, now, now)
	return err
}

func (s *Store) SetDisplayName(deviceID, displayName string) error {
	if err := s.EnsureDevice(deviceID); err != nil {
		return err
	}
	_, err := s.db.Exec(`
		UPDATE devices SET display_name = ?, updated_at = ?
		WHERE device_id = ?`,
		displayName, time.Now().UTC().Format(time.RFC3339), deviceID)
	return err
}

func (s *Store) SetNotes(deviceID, notes string) error {
	if err := s.EnsureDevice(deviceID); err != nil {
		return err
	}
	_, err := s.db.Exec(`
		UPDATE devices SET notes = ?, updated_at = ?
		WHERE device_id = ?`,
		notes, time.Now().UTC().Format(time.RFC3339), deviceID)
	return err
}

// SetAttribute upserts one point-in-time key/value fact for a device —
// overwrites the previous value for that key, never accumulates. value is
// always stored as text; SQLite doesn't enforce column typing, so this
// covers numeric and string attributes uniformly.
func (s *Store) SetAttribute(deviceID, key, value string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(`
		INSERT INTO device_attributes (device_id, key, value, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (device_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		deviceID, key, value, now)
	return err
}

// GetAttributes returns every known key/value attribute for a device.
func (s *Store) GetAttributes(deviceID string) (map[string]string, error) {
	rows, err := s.db.Query(`
		SELECT key, value FROM device_attributes WHERE device_id = ?`,
		deviceID)
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

// DeviceIPsForTenant returns the current tailscale_ip attribute of every
// device currently assigned to tenantID that has reported one — used to
// build that tenant's Tailscale ACL grant (see internal/tailscaleapi),
// IP-based rather than tag-based (docs/MULTI-TENANCY.md Tappa 3b). A
// device that's never published yet, or is offline long enough to have no
// attribute row, simply isn't in the list — no entry to remove, nothing
// to break. Joins to devices since tenant assignment isn't stored on
// device_attributes itself (see package doc) — a device's tenant is
// always looked up live from devices.tenant_id, so a reassignment takes
// effect here immediately, no separate sync step.
func (s *Store) DeviceIPsForTenant(tenantID string) ([]string, error) {
	rows, err := s.db.Query(`
		SELECT a.value
		FROM device_attributes a
		JOIN devices d ON d.device_id = a.device_id
		WHERE d.tenant_id = ? AND a.key = 'tailscale_ip' AND a.value != ''`,
		tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			return nil, err
		}
		out = append(out, ip)
	}
	return out, rows.Err()
}

// DeviceIDsForTenant returns every device_id currently assigned to
// tenantID — the enforcement primitive the LLM tool API uses to keep a
// tool call scoped to the caller's tenant regardless of what device_id an
// LLM might pass as an argument (never trust that alone — see
// docs/LLM-INTEGRATION.md). Unlike DeviceIPsForTenant this doesn't
// require the device to have ever reported an attribute — a device
// belongs to its tenant purely by the devices.tenant_id column.
func (s *Store) DeviceIDsForTenant(tenantID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT device_id FROM devices WHERE tenant_id = ?`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SaveRecord upserts a full snapshot tied to one occurrence (recordID) —
// unlike SetAttribute, this accumulates one row per distinct recordID
// instead of overwriting a single current value; publishing the same
// recordID again replaces just that occurrence (idempotent retry), not
// history. data is stored as a JSON object (SQLite has no strict column
// typing, and Grafana's SQLite plugin can query it with json_extract()).
func (s *Store) SaveRecord(deviceID, recordID string, ts time.Time, data map[string]string) error {
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO device_records (device_id, record_id, ts, data_json)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (device_id, record_id) DO UPDATE SET ts = excluded.ts, data_json = excluded.data_json`,
		deviceID, recordID, ts.UTC().Format(time.RFC3339), string(dataJSON))
	return err
}

// RecordMeta is one row from device_records, with Data already decoded.
type RecordMeta struct {
	DeviceID string
	RecordID string
	Data     map[string]string
}

// FindRecordsByFilters returns every record (any device) matching ALL of
// the given field=value pairs (AND-ed) — e.g. {"treatment_id": X, "kind":
// "topography"} for just one category of file linked to one treatment. A
// field can be one of device_records' real columns (device_id, record_id,
// ts) or, for anything else, a key inside data_json — same filter syntax
// either way, no code change needed to filter on a new data field since
// that side is schema-less by design. filters must be non-empty. Field
// names are restricted to letters/digits/underscore before use: harmless
// either way (bound parameters, not SQL-injectable — column names are the
// one exception, see below), but an unexpected JSON path expression would
// just be a confusing correctness bug for no reason.
func (s *Store) FindRecordsByFilters(filters map[string]string) ([]RecordMeta, error) {
	if len(filters) == 0 {
		return nil, fmt.Errorf("store: at least one filter required")
	}

	// Sorted purely so the generated SQL (and therefore server logs) is
	// deterministic across calls with the same filters — Go map iteration
	// order isn't, but the AND-conjunction's meaning doesn't depend on it.
	fields := make([]string, 0, len(filters))
	for f := range filters {
		fields = append(fields, f)
	}
	sort.Strings(fields)

	var conditions []string
	var args []any
	for _, field := range fields {
		for _, r := range field {
			if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
				return nil, fmt.Errorf("store: invalid field name %q", field)
			}
		}
		if recordTopLevelColumns[field] {
			// field is validated alphanumeric/underscore above, so this
			// concatenation can't smuggle in SQL syntax — it's still a
			// real column name, not a bound value, which is the one
			// place field names can't just be parameters.
			conditions = append(conditions, field+" = ?")
			args = append(args, filters[field])
		} else {
			conditions = append(conditions, "json_extract(data_json, ?) = ?")
			args = append(args, "$."+field, filters[field])
		}
	}

	query := `SELECT device_id, record_id, data_json FROM device_records WHERE ` +
		strings.Join(conditions, " AND ") + ` ORDER BY ts DESC`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RecordMeta
	for rows.Next() {
		var m RecordMeta
		var dataJSON string
		if err := rows.Scan(&m.DeviceID, &m.RecordID, &dataJSON); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(dataJSON), &m.Data); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Tenant is a registered tenant — see docs/MULTI-TENANCY.md for the design
// this is part of. tenant_id is chosen by whoever creates the tenant (an
// admin, via a short human-meaningful slug) — unlike device_id, which a
// device chooses for itself (see internal/telemetry package doc).
type Tenant struct {
	TenantID    string
	DisplayName string
	CreatedAt   time.Time
}

// CreateTenant registers a new tenant with an admin-chosen tenant_id.
// Returns an error if that tenant_id is already taken.
func (s *Store) CreateTenant(tenantID, displayName string) error {
	res, err := s.db.Exec(`
		INSERT INTO tenants (tenant_id, display_name, created_at)
		VALUES (?, ?, ?)
		ON CONFLICT (tenant_id) DO NOTHING`,
		tenantID, displayName, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: tenant %q already exists", tenantID)
	}
	return nil
}

// DeleteTenant removes tenantID and everything owned by it —
// tenant_members, tenant_grafana_orgs — and moves any device still
// assigned to it back to DefaultTenantID first, so no device is ever left
// pointing at a tenant that no longer exists. Refuses to delete
// DefaultTenantID itself, since that's the bootstrap fallback every new
// device lands in. Returns the former Grafana org id (if one was
// provisioned) so the caller can clean it up via the Grafana API — this
// store has no HTTP client of its own.
func (s *Store) DeleteTenant(tenantID string) (grafanaOrgID int64, hadOrg bool, err error) {
	if tenantID == DefaultTenantID {
		return 0, false, fmt.Errorf("store: cannot delete the default tenant")
	}
	grafanaOrgID, hadOrg, err = s.GetTenantGrafanaOrg(tenantID)
	if err != nil {
		return 0, false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.Exec(`
		UPDATE devices SET tenant_id = ?, updated_at = ? WHERE tenant_id = ?`,
		DefaultTenantID, now, tenantID); err != nil {
		return 0, false, err
	}
	for _, stmt := range []string{
		`DELETE FROM tenant_members WHERE tenant_id = ?`,
		`DELETE FROM tenant_grafana_orgs WHERE tenant_id = ?`,
		`DELETE FROM tenants WHERE tenant_id = ?`,
	} {
		if _, err := tx.Exec(stmt, tenantID); err != nil {
			return 0, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return grafanaOrgID, hadOrg, nil
}

// ReassignDevice moves deviceID to a different tenant — a single column
// update: tenant assignment is the only thing that's ever tenant-scoped
// on a device (see package doc), there's no history or wire-format state
// tied to the old tenant to reconcile. Errors if newTenantID doesn't
// exist — a device should never end up pointing at a tenant that isn't
// real — or if deviceID doesn't exist.
func (s *Store) ReassignDevice(deviceID, newTenantID string) error {
	var exists int
	if err := s.db.QueryRow(`SELECT 1 FROM tenants WHERE tenant_id = ?`, newTenantID).Scan(&exists); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("store: tenant %q does not exist", newTenantID)
		}
		return err
	}

	res, err := s.db.Exec(`
		UPDATE devices SET tenant_id = ?, updated_at = ?
		WHERE device_id = ?`,
		newTenantID, time.Now().UTC().Format(time.RFC3339), deviceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: device %q not found", deviceID)
	}
	return nil
}

// ListTenants returns every registered tenant, most recently created first.
func (s *Store) ListTenants() ([]Tenant, error) {
	rows, err := s.db.Query(`SELECT tenant_id, display_name, created_at FROM tenants ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Tenant
	for rows.Next() {
		var t Tenant
		var createdAt string
		if err := rows.Scan(&t.TenantID, &t.DisplayName, &createdAt); err != nil {
			return nil, err
		}
		t.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		out = append(out, t)
	}
	return out, rows.Err()
}

// Valid tenant_members roles — see docs/MULTI-TENANCY.md "Sistema di
// ruoli/permessi". Kept as a small fixed set here (not an enum type)
// since it's validated in exactly one place (AddTenantMember) and used
// as plain strings everywhere else (Grafana role names, UI labels).
const (
	RoleViewer   = "viewer"
	RoleOperator = "operator"
)

// TenantMember is one row of tenant_members — a login's membership in one
// tenant, with the role that determines what they can do there (see
// docs/MULTI-TENANCY.md).
type TenantMember struct {
	Login string
	Role  string
}

// AddTenantMember grants login (a Tailscale login/email — see
// tailscaleip.WhoIs) access to tenantID with the given role. A login can
// belong to more than one tenant — this is a membership row, not a single
// "the" tenant field, so adding a second one is just another insert, not
// a conflict. Adding an already-member login updates their role instead
// of erroring — same "set membership" intent either way.
func (s *Store) AddTenantMember(tenantID, login, role string) error {
	if role != RoleViewer && role != RoleOperator {
		return fmt.Errorf("store: invalid role %q", role)
	}
	_, err := s.db.Exec(`
		INSERT INTO tenant_members (tenant_id, login, role, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (tenant_id, login) DO UPDATE SET role = excluded.role`,
		tenantID, login, role, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Store) RemoveTenantMember(tenantID, login string) error {
	_, err := s.db.Exec(`DELETE FROM tenant_members WHERE tenant_id = ? AND login = ?`, tenantID, login)
	return err
}

// TenantsForLogin returns every tenant_id login is a member of.
func (s *Store) TenantsForLogin(login string) ([]string, error) {
	rows, err := s.db.Query(`SELECT tenant_id FROM tenant_members WHERE login = ?`, login)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RoleForLogin returns login's role in tenantID, or ("", false) if
// they're not a member at all — used to decide what to show/allow (e.g.
// hiding the VNC/SSH link from viewers, see internal/dashboard).
func (s *Store) RoleForLogin(tenantID, login string) (role string, ok bool, err error) {
	row := s.db.QueryRow(`SELECT role FROM tenant_members WHERE tenant_id = ? AND login = ?`, tenantID, login)
	err = row.Scan(&role)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return role, true, nil
}

// ListTenantMembers returns every member of tenantID with their role.
func (s *Store) ListTenantMembers(tenantID string) ([]TenantMember, error) {
	rows, err := s.db.Query(`SELECT login, role FROM tenant_members WHERE tenant_id = ? ORDER BY login`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TenantMember
	for rows.Next() {
		var m TenantMember
		if err := rows.Scan(&m.Login, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetTenantGrafanaOrg records which Grafana Org id a tenant was
// provisioned into (see internal/grafana) — one row per tenant, set once.
func (s *Store) SetTenantGrafanaOrg(tenantID string, orgID int64) error {
	_, err := s.db.Exec(`
		INSERT INTO tenant_grafana_orgs (tenant_id, org_id, created_at)
		VALUES (?, ?, ?)
		ON CONFLICT (tenant_id) DO UPDATE SET org_id = excluded.org_id`,
		tenantID, orgID, time.Now().UTC().Format(time.RFC3339))
	return err
}

// GetTenantGrafanaOrg returns the Grafana Org id for tenantID, if any.
func (s *Store) GetTenantGrafanaOrg(tenantID string) (orgID int64, ok bool, err error) {
	row := s.db.QueryRow(`SELECT org_id FROM tenant_grafana_orgs WHERE tenant_id = ?`, tenantID)
	err = row.Scan(&orgID)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return orgID, true, nil
}

// ListAllDevices returns every device ever persisted, across every
// tenant — the dashboard's device list (see cmd/farsight-server's
// listWithMeta) enumerates from this, not from the live in-memory
// registry, specifically so a device that isn't currently
// online/connected (imported once via a batch upload, not a
// continuously-running agent — see examples/generic-file-import) still
// shows up, just as offline, instead of disappearing from the list
// entirely after the next farsight-server restart clears the registry.
// filterForIdentity applies tenant scoping afterward, same as it always
// has — this returns everything, unfiltered.
func (s *Store) ListAllDevices() ([]DeviceMeta, error) {
	rows, err := s.db.Query(`SELECT device_id, tenant_id, display_name, notes, created_at, updated_at FROM devices`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DeviceMeta
	for rows.Next() {
		var m DeviceMeta
		var createdAt, updatedAt string
		if err := rows.Scan(&m.DeviceID, &m.TenantID, &m.DisplayName, &m.Notes, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		m.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		m.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetDevice returns the persisted metadata for deviceID, or a zero-value
// DeviceMeta (no error, TenantID left empty) if it isn't known yet —
// callers merge this with live registry state, and a device can be live
// before it has ever been persisted.
func (s *Store) GetDevice(deviceID string) (DeviceMeta, error) {
	var m DeviceMeta
	var createdAt, updatedAt string
	row := s.db.QueryRow(`
		SELECT device_id, tenant_id, display_name, notes, created_at, updated_at
		FROM devices WHERE device_id = ?`, deviceID)
	err := row.Scan(&m.DeviceID, &m.TenantID, &m.DisplayName, &m.Notes, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		m.DeviceID = deviceID
		return m, nil
	}
	if err != nil {
		return DeviceMeta{}, err
	}
	m.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	m.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return m, nil
}

// Conversation is one LLM chat thread — private to the login that created
// it, scoped to one tenant (see docs/LLM-INTEGRATION.md "Chat history" —
// not shared tenant-wide, an operator doesn't see another operator's
// conversations).
type Conversation struct {
	ID        int64
	TenantID  string
	Login     string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Message is one turn of a Conversation — a plain chat message
// (ToolName/ToolArgs/ToolResult empty) or a tool call/result (Content
// empty, the other three set) — stored either way so a conversation can
// be replayed/debugged in full, not just its visible text.
type Message struct {
	ID             int64
	ConversationID int64
	Role           string
	Content        string
	ToolName       string
	ToolArgs       string
	ToolResult     string
	CreatedAt      time.Time
}

// CreateConversation starts a new, empty conversation for login in
// tenantID.
func (s *Store) CreateConversation(tenantID, login, title string) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.Exec(`
		INSERT INTO llm_conversations (tenant_id, login, title, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`,
		tenantID, login, title, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListConversations returns login's own conversations in tenantID, most
// recently updated first.
func (s *Store) ListConversations(tenantID, login string) ([]Conversation, error) {
	rows, err := s.db.Query(`
		SELECT id, tenant_id, login, title, created_at, updated_at
		FROM llm_conversations WHERE tenant_id = ? AND login = ?
		ORDER BY updated_at DESC`, tenantID, login)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Conversation
	for rows.Next() {
		var c Conversation
		var createdAt, updatedAt string
		if err := rows.Scan(&c.ID, &c.TenantID, &c.Login, &c.Title, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		c.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		c.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetConversation returns id, but only if it belongs to tenantID and
// login — the ownership check is baked into the query itself (not a
// separate "does this belong to them" check after the fact) so a guessed
// or leaked conversation id from another tenant/login can never be read.
func (s *Store) GetConversation(id int64, tenantID, login string) (Conversation, error) {
	var c Conversation
	var createdAt, updatedAt string
	row := s.db.QueryRow(`
		SELECT id, tenant_id, login, title, created_at, updated_at
		FROM llm_conversations WHERE id = ? AND tenant_id = ? AND login = ?`,
		id, tenantID, login)
	err := row.Scan(&c.ID, &c.TenantID, &c.Login, &c.Title, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return Conversation{}, fmt.Errorf("store: conversation %d not found", id)
	}
	if err != nil {
		return Conversation{}, err
	}
	c.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	c.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return c, nil
}

// DeleteConversation removes a conversation and its messages — same
// ownership-in-the-WHERE-clause pattern as GetConversation, errors if id
// doesn't belong to tenantID/login (including if it simply doesn't
// exist — indistinguishable on purpose, doesn't leak which).
func (s *Store) DeleteConversation(id int64, tenantID, login string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`DELETE FROM llm_conversations WHERE id = ? AND tenant_id = ? AND login = ?`,
		id, tenantID, login)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: conversation %d not found", id)
	}
	if _, err := tx.Exec(`DELETE FROM llm_messages WHERE conversation_id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// AppendMessage adds one turn to conversationID and bumps the
// conversation's updated_at (so ListConversations sorts active threads
// first). Callers pass either (role, content) for a plain chat message or
// (role="tool", toolName, toolArgs, toolResult) for a tool call/result —
// this method doesn't validate which, it just stores what it's given.
func (s *Store) AppendMessage(conversationID int64, role, content, toolName, toolArgs, toolResult string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		INSERT INTO llm_messages (conversation_id, role, content, tool_name, tool_args, tool_result, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		conversationID, role, content, toolName, toolArgs, toolResult, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE llm_conversations SET updated_at = ? WHERE id = ?`, now, conversationID); err != nil {
		return err
	}
	return tx.Commit()
}

// ListMessages returns every message of conversationID in order — no
// ownership check here on purpose, callers are expected to have already
// resolved/verified the conversation via GetConversation first.
func (s *Store) ListMessages(conversationID int64) ([]Message, error) {
	rows, err := s.db.Query(`
		SELECT id, conversation_id, role, content, tool_name, tool_args, tool_result, created_at
		FROM llm_messages WHERE conversation_id = ? ORDER BY id ASC`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var m Message
		var createdAt string
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content,
			&m.ToolName, &m.ToolArgs, &m.ToolResult, &createdAt); err != nil {
			return nil, err
		}
		m.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ToolCallLogEntry is one row of the admin-visible audit trail — which
// tool was called, by whom, when. Deliberately separate from Message: an
// admin can see that a tool ran without being able to read the full
// conversation content it ran inside (see docs/LLM-INTEGRATION.md "Chat
// history" — conversation content is private, this log isn't).
type ToolCallLogEntry struct {
	TenantID  string
	Login     string
	ToolName  string
	CreatedAt time.Time
}

// LogToolCall records one tool invocation for the audit log.
func (s *Store) LogToolCall(tenantID, login, toolName string) error {
	_, err := s.db.Exec(`
		INSERT INTO llm_tool_calls (tenant_id, login, tool_name, created_at)
		VALUES (?, ?, ?, ?)`,
		tenantID, login, toolName, time.Now().UTC().Format(time.RFC3339))
	return err
}

// ListToolCallLog returns the most recent tool-call audit entries across
// every tenant (admin-only view — see cmd/farsight-server's requireAdmin),
// newest first, capped at limit.
func (s *Store) ListToolCallLog(limit int) ([]ToolCallLogEntry, error) {
	rows, err := s.db.Query(`
		SELECT tenant_id, login, tool_name, created_at
		FROM llm_tool_calls ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ToolCallLogEntry
	for rows.Next() {
		var e ToolCallLogEntry
		var createdAt string
		if err := rows.Scan(&e.TenantID, &e.Login, &e.ToolName, &createdAt); err != nil {
			return nil, err
		}
		e.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetSetting returns key's current value from the generic admin-editable
// settings table (used for LLM config: model tier, system prompt,
// Grafana LLM API key, metric descriptions — see cmd/farsight-server's
// /llm-settings page) — ok is false if it's never been set. Read live on
// every use, not cached: an admin editing a setting takes effect
// immediately, same "no restart needed" property as tenant/admin
// management already has.
func (s *Store) GetSetting(key string) (value string, ok bool, err error) {
	row := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key)
	err = row.Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// GetSettingOr is GetSetting with a fallback for the common case of
// "use this default if never configured."
func (s *Store) GetSettingOr(key, fallback string) (string, error) {
	v, ok, err := s.GetSetting(key)
	if err != nil {
		return "", err
	}
	if !ok {
		return fallback, nil
	}
	return v, nil
}

// SetSetting upserts key's value.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`
		INSERT INTO settings (key, value, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().UTC().Format(time.RFC3339))
	return err
}

// SetSettingIfAbsent sets key's value only if it has never been set —
// used to seed settings from server.conf on first boot (see main.go)
// without ever overwriting an admin's later UI edit on a subsequent
// restart.
func (s *Store) SetSettingIfAbsent(key, value string) error {
	if value == "" {
		return nil
	}
	_, ok, err := s.GetSetting(key)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	return s.SetSetting(key, value)
}

const metricDescriptionsSettingKey = "llm_metric_descriptions"

// GetMetricDescriptions returns the admin-configured metric_name ->
// description map (see the /llm-settings page) — empty map, not an
// error, if none configured yet. Stored as one JSON blob under a single
// settings key rather than one row per metric: it's edited as a whole
// list on one admin page, not queried per-metric anywhere.
func (s *Store) GetMetricDescriptions() (map[string]string, error) {
	raw, ok, err := s.GetSetting(metricDescriptionsSettingKey)
	if err != nil {
		return nil, err
	}
	if !ok {
		return map[string]string{}, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return map[string]string{}, nil
	}
	return m, nil
}

// SetMetricDescription adds or updates one metric's description —
// read-modify-write on the single JSON blob, fine at the scale this
// operates at (an admin editing a handful of entries, not a hot path).
func (s *Store) SetMetricDescription(name, description string) error {
	m, err := s.GetMetricDescriptions()
	if err != nil {
		return err
	}
	m[name] = description
	return s.saveMetricDescriptions(m)
}

// RemoveMetricDescription deletes one metric's description, if present.
func (s *Store) RemoveMetricDescription(name string) error {
	m, err := s.GetMetricDescriptions()
	if err != nil {
		return err
	}
	delete(m, name)
	return s.saveMetricDescriptions(m)
}

func (s *Store) saveMetricDescriptions(m map[string]string) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.SetSetting(metricDescriptionsSettingKey, string(raw))
}
