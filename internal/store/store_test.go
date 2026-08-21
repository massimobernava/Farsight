package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpen(t *testing.T) {
	t.Run("bootstraps the default tenant", func(t *testing.T) {
		s := newTestStore(t)
		tenants, err := s.ListTenants()
		if err != nil {
			t.Fatalf("ListTenants: %v", err)
		}
		if len(tenants) != 1 || tenants[0].TenantID != DefaultTenantID {
			t.Fatalf("expected only %q, got %+v", DefaultTenantID, tenants)
		}
	})

	t.Run("is idempotent (reopening an existing file doesn't fail or duplicate the default tenant)", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.db")
		s1, err := Open(path)
		if err != nil {
			t.Fatalf("first Open: %v", err)
		}
		s1.Close()

		s2, err := Open(path)
		if err != nil {
			t.Fatalf("second Open: %v", err)
		}
		defer s2.Close()

		tenants, err := s2.ListTenants()
		if err != nil {
			t.Fatalf("ListTenants: %v", err)
		}
		if len(tenants) != 1 {
			t.Fatalf("expected exactly 1 tenant after reopen, got %d", len(tenants))
		}
	})

	t.Run("rejects a database with the old composite-key devices schema", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "old.db")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		// Old shape: composite (tenant_id, device_id) primary key.
		if _, err := db.Exec(`CREATE TABLE devices (
			tenant_id TEXT NOT NULL, device_id TEXT NOT NULL,
			PRIMARY KEY (tenant_id, device_id)
		)`); err != nil {
			t.Fatalf("create old table: %v", err)
		}
		db.Close()

		_, err = Open(path)
		if err == nil {
			t.Fatal("expected Open to reject the old schema, got nil error")
		}
	})
}

// TestConcurrentWrites exercises the exact failure mode observed for real
// on zeus ("database is locked (5) (SQLITE_BUSY)", see docs/BACKLOG.md):
// many goroutines writing through the same *Store at once, some to the
// same row (EnsureDevice on one shared device_id, which upserts) and some
// to distinct rows (SetAttribute), mixed with concurrent reads — the shape
// of real MQTT-driven writes racing HTTP-driven reads. Open's WAL mode +
// busy_timeout (see store.go) should absorb all of this without a single
// SQLITE_BUSY surfacing; a regression here (e.g. someone dropping the
// PRAGMA calls) should turn this red.
func TestConcurrentWrites(t *testing.T) {
	s := newTestStore(t)
	const goroutines = 40
	const opsEach = 25

	var wg sync.WaitGroup
	errs := make(chan error, goroutines*opsEach*3) // 3 fallible calls per iteration below

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			deviceID := fmt.Sprintf("dev-%d", g%5) // deliberate overlap: forces real contention on shared rows
			for i := 0; i < opsEach; i++ {
				if err := s.EnsureDevice(deviceID); err != nil {
					errs <- fmt.Errorf("EnsureDevice: %w", err)
				}
				if err := s.SetAttribute(deviceID, fmt.Sprintf("key-%d", g), fmt.Sprintf("value-%d", i)); err != nil {
					errs <- fmt.Errorf("SetAttribute: %w", err)
				}
				if _, err := s.GetAttributes(deviceID); err != nil {
					errs <- fmt.Errorf("GetAttributes: %w", err)
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)

	var got []error
	for err := range errs {
		got = append(got, err)
	}
	if len(got) != 0 {
		t.Fatalf("%d/%d concurrent operations failed, first: %v", len(got), goroutines*opsEach*3, got[0])
	}
}

func TestAdmins(t *testing.T) {
	s := newTestStore(t)

	t.Run("starts with no admins", func(t *testing.T) {
		admins, err := s.ListAdmins()
		if err != nil {
			t.Fatalf("ListAdmins: %v", err)
		}
		if len(admins) != 0 {
			t.Fatalf("expected no admins, got %v", admins)
		}
		if ok, err := s.IsAdmin("nobody@example.com"); err != nil || ok {
			t.Fatalf("IsAdmin(unknown) = %v, %v; want false, nil", ok, err)
		}
	})

	t.Run("AddAdmin/IsAdmin/RemoveAdmin round-trip", func(t *testing.T) {
		if err := s.AddAdmin("alice@example.com"); err != nil {
			t.Fatalf("AddAdmin: %v", err)
		}
		if ok, err := s.IsAdmin("alice@example.com"); err != nil || !ok {
			t.Fatalf("IsAdmin(alice) = %v, %v; want true, nil", ok, err)
		}
		if err := s.RemoveAdmin("alice@example.com"); err != nil {
			t.Fatalf("RemoveAdmin: %v", err)
		}
		if ok, _ := s.IsAdmin("alice@example.com"); ok {
			t.Fatal("expected alice to no longer be admin after RemoveAdmin")
		}
	})

	t.Run("AddAdmin is idempotent", func(t *testing.T) {
		if err := s.AddAdmin("bob@example.com"); err != nil {
			t.Fatalf("first AddAdmin: %v", err)
		}
		if err := s.AddAdmin("bob@example.com"); err != nil {
			t.Fatalf("second AddAdmin: %v", err)
		}
		admins, _ := s.ListAdmins()
		count := 0
		for _, a := range admins {
			if a == "bob@example.com" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("expected bob listed exactly once, got %d times in %v", count, admins)
		}
	})

	t.Run("ListAdmins is alphabetical", func(t *testing.T) {
		s2 := newTestStore(t)
		for _, login := range []string{"zeta@example.com", "alpha@example.com", "mid@example.com"} {
			if err := s2.AddAdmin(login); err != nil {
				t.Fatalf("AddAdmin(%q): %v", login, err)
			}
		}
		got, err := s2.ListAdmins()
		if err != nil {
			t.Fatalf("ListAdmins: %v", err)
		}
		want := []string{"alpha@example.com", "mid@example.com", "zeta@example.com"}
		if !equalStrings(got, want) {
			t.Fatalf("ListAdmins = %v, want %v", got, want)
		}
	})

	t.Run("BootstrapAdmins seeds only when empty", func(t *testing.T) {
		s2 := newTestStore(t)
		if err := s2.BootstrapAdmins([]string{"seed1@example.com", "seed2@example.com"}); err != nil {
			t.Fatalf("BootstrapAdmins: %v", err)
		}
		admins, _ := s2.ListAdmins()
		if len(admins) != 2 {
			t.Fatalf("expected 2 seeded admins, got %v", admins)
		}

		// A second BootstrapAdmins call with different logins must be a no-op
		// once the table is non-empty (see BootstrapAdmins doc comment).
		if err := s2.BootstrapAdmins([]string{"different@example.com"}); err != nil {
			t.Fatalf("second BootstrapAdmins: %v", err)
		}
		admins, _ = s2.ListAdmins()
		if len(admins) != 2 {
			t.Fatalf("expected BootstrapAdmins to no-op on a non-empty table, got %v", admins)
		}
	})
}

func TestDevices(t *testing.T) {
	s := newTestStore(t)

	t.Run("EnsureDevice creates a device in the default tenant", func(t *testing.T) {
		if err := s.EnsureDevice("dev-1"); err != nil {
			t.Fatalf("EnsureDevice: %v", err)
		}
		d, err := s.GetDevice("dev-1")
		if err != nil {
			t.Fatalf("GetDevice: %v", err)
		}
		if d.TenantID != DefaultTenantID {
			t.Fatalf("TenantID = %q, want %q", d.TenantID, DefaultTenantID)
		}
	})

	t.Run("EnsureDevice is idempotent", func(t *testing.T) {
		if err := s.EnsureDevice("dev-1"); err != nil {
			t.Fatalf("EnsureDevice (again): %v", err)
		}
		all, err := s.ListAllDevices()
		if err != nil {
			t.Fatalf("ListAllDevices: %v", err)
		}
		count := 0
		for _, d := range all {
			if d.DeviceID == "dev-1" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("expected dev-1 exactly once, got %d times", count)
		}
	})

	t.Run("GetDevice on an unknown device returns a zero-value, not an error", func(t *testing.T) {
		d, err := s.GetDevice("never-seen")
		if err != nil {
			t.Fatalf("GetDevice: %v", err)
		}
		if d.DeviceID != "never-seen" || d.TenantID != "" {
			t.Fatalf("GetDevice(unknown) = %+v, want DeviceID set and everything else zero", d)
		}
	})

	t.Run("SetDisplayName/SetNotes implicitly ensure the device first", func(t *testing.T) {
		if err := s.SetDisplayName("dev-2", "My Device"); err != nil {
			t.Fatalf("SetDisplayName: %v", err)
		}
		if err := s.SetNotes("dev-2", "some notes"); err != nil {
			t.Fatalf("SetNotes: %v", err)
		}
		d, err := s.GetDevice("dev-2")
		if err != nil {
			t.Fatalf("GetDevice: %v", err)
		}
		if d.DisplayName != "My Device" || d.Notes != "some notes" {
			t.Fatalf("GetDevice(dev-2) = %+v, want DisplayName=%q Notes=%q", d, "My Device", "some notes")
		}
	})

	t.Run("ListAllDevices returns every device across tenants", func(t *testing.T) {
		s2 := newTestStore(t)
		if err := s2.CreateTenant("t1", "Tenant 1"); err != nil {
			t.Fatalf("CreateTenant: %v", err)
		}
		if err := s2.EnsureDevice("d-default"); err != nil {
			t.Fatalf("EnsureDevice: %v", err)
		}
		if err := s2.EnsureDevice("d-t1"); err != nil {
			t.Fatalf("EnsureDevice: %v", err)
		}
		if err := s2.ReassignDevice("d-t1", "t1"); err != nil {
			t.Fatalf("ReassignDevice: %v", err)
		}
		all, err := s2.ListAllDevices()
		if err != nil {
			t.Fatalf("ListAllDevices: %v", err)
		}
		if len(all) != 2 {
			t.Fatalf("expected 2 devices, got %d: %+v", len(all), all)
		}
	})
}

func TestAttributes(t *testing.T) {
	s := newTestStore(t)

	t.Run("GetAttributes on a device with none returns an empty map", func(t *testing.T) {
		attrs, err := s.GetAttributes("no-attrs")
		if err != nil {
			t.Fatalf("GetAttributes: %v", err)
		}
		if len(attrs) != 0 {
			t.Fatalf("expected empty map, got %v", attrs)
		}
	})

	t.Run("SetAttribute upserts (overwrites, doesn't accumulate)", func(t *testing.T) {
		if err := s.SetAttribute("dev-1", "firmware", "1.0"); err != nil {
			t.Fatalf("SetAttribute: %v", err)
		}
		if err := s.SetAttribute("dev-1", "firmware", "1.1"); err != nil {
			t.Fatalf("SetAttribute (overwrite): %v", err)
		}
		attrs, err := s.GetAttributes("dev-1")
		if err != nil {
			t.Fatalf("GetAttributes: %v", err)
		}
		if attrs["firmware"] != "1.1" {
			t.Fatalf("firmware = %q, want %q", attrs["firmware"], "1.1")
		}
		if len(attrs) != 1 {
			t.Fatalf("expected exactly 1 attribute (overwritten, not accumulated), got %v", attrs)
		}
	})

	t.Run("DeviceIPsForTenant only returns devices with a tailscale_ip attribute, scoped to the tenant", func(t *testing.T) {
		s2 := newTestStore(t)
		must(t, s2.CreateTenant("t1", "Tenant 1"))
		must(t, s2.EnsureDevice("with-ip"))
		must(t, s2.EnsureDevice("without-ip"))
		must(t, s2.EnsureDevice("other-tenant"))
		must(t, s2.ReassignDevice("with-ip", "t1"))
		must(t, s2.ReassignDevice("without-ip", "t1"))
		// other-tenant stays in "default" on purpose.
		must(t, s2.SetAttribute("with-ip", "tailscale_ip", "100.1.2.3"))
		must(t, s2.SetAttribute("other-tenant", "tailscale_ip", "100.9.9.9"))

		ips, err := s2.DeviceIPsForTenant("t1")
		if err != nil {
			t.Fatalf("DeviceIPsForTenant: %v", err)
		}
		if !equalStrings(ips, []string{"100.1.2.3"}) {
			t.Fatalf("DeviceIPsForTenant(t1) = %v, want [100.1.2.3]", ips)
		}
	})

	t.Run("DeviceIDsForTenant scopes by devices.tenant_id regardless of attributes", func(t *testing.T) {
		s2 := newTestStore(t)
		must(t, s2.CreateTenant("t1", "Tenant 1"))
		must(t, s2.EnsureDevice("a"))
		must(t, s2.EnsureDevice("b"))
		must(t, s2.ReassignDevice("a", "t1"))

		ids, err := s2.DeviceIDsForTenant("t1")
		if err != nil {
			t.Fatalf("DeviceIDsForTenant: %v", err)
		}
		if !equalStrings(ids, []string{"a"}) {
			t.Fatalf("DeviceIDsForTenant(t1) = %v, want [a]", ids)
		}
	})
}

func TestRecords(t *testing.T) {
	t.Run("SaveRecord upserts by (device_id, record_id)", func(t *testing.T) {
		s := newTestStore(t)
		must(t, s.SaveRecord("dev-1", "rec-1", time.Now(), map[string]string{"score": "0.5"}))
		must(t, s.SaveRecord("dev-1", "rec-1", time.Now(), map[string]string{"score": "0.9"}))

		got, err := s.FindRecordsByFilters(map[string]string{"record_id": "rec-1"})
		if err != nil {
			t.Fatalf("FindRecordsByFilters: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 record after upsert, got %d: %+v", len(got), got)
		}
		if got[0].Data["score"] != "0.9" {
			t.Fatalf("score = %q, want %q (latest write should win)", got[0].Data["score"], "0.9")
		}
	})

	t.Run("FindRecordsByFilters rejects an empty filter set", func(t *testing.T) {
		s := newTestStore(t)
		if _, err := s.FindRecordsByFilters(nil); err == nil {
			t.Fatal("expected an error for empty filters, got nil")
		}
	})

	t.Run("FindRecordsByFilters rejects an invalid field name", func(t *testing.T) {
		s := newTestStore(t)
		if _, err := s.FindRecordsByFilters(map[string]string{"bad field; DROP TABLE devices": "x"}); err == nil {
			t.Fatal("expected an error for an invalid field name, got nil")
		}
	})

	t.Run("FindRecordsByFilters ANDs a mix of top-level columns and JSON fields", func(t *testing.T) {
		s := newTestStore(t)
		must(t, s.SaveRecord("dev-1", "rec-1", time.Now(), map[string]string{"kind": "image", "patient": "P1"}))
		must(t, s.SaveRecord("dev-1", "rec-2", time.Now(), map[string]string{"kind": "image", "patient": "P2"}))
		must(t, s.SaveRecord("dev-2", "rec-3", time.Now(), map[string]string{"kind": "image", "patient": "P1"}))

		got, err := s.FindRecordsByFilters(map[string]string{"device_id": "dev-1", "kind": "image"})
		if err != nil {
			t.Fatalf("FindRecordsByFilters: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("expected 2 matching records, got %d: %+v", len(got), got)
		}

		got, err = s.FindRecordsByFilters(map[string]string{"device_id": "dev-1", "patient": "P1"})
		if err != nil {
			t.Fatalf("FindRecordsByFilters: %v", err)
		}
		if len(got) != 1 || got[0].RecordID != "rec-1" {
			t.Fatalf("expected only rec-1, got %+v", got)
		}
	})
}

func TestTenants(t *testing.T) {
	t.Run("CreateTenant rejects a duplicate tenant_id", func(t *testing.T) {
		s := newTestStore(t)
		must(t, s.CreateTenant("acme", "Acme"))
		if err := s.CreateTenant("acme", "Acme Again"); err == nil {
			t.Fatal("expected an error creating a duplicate tenant, got nil")
		}
	})

	t.Run("DeleteTenant refuses to delete the default tenant", func(t *testing.T) {
		s := newTestStore(t)
		if _, _, err := s.DeleteTenant(DefaultTenantID); err == nil {
			t.Fatal("expected an error deleting the default tenant, got nil")
		}
	})

	t.Run("DeleteTenant moves its devices back to default and cleans up members/org", func(t *testing.T) {
		s := newTestStore(t)
		must(t, s.CreateTenant("acme", "Acme"))
		must(t, s.EnsureDevice("dev-1"))
		must(t, s.ReassignDevice("dev-1", "acme"))
		must(t, s.AddTenantMember("acme", "alice@example.com", RoleViewer))
		must(t, s.SetTenantGrafanaOrg("acme", 42))

		orgID, hadOrg, err := s.DeleteTenant("acme")
		if err != nil {
			t.Fatalf("DeleteTenant: %v", err)
		}
		if !hadOrg || orgID != 42 {
			t.Fatalf("DeleteTenant returned orgID=%d hadOrg=%v, want 42, true", orgID, hadOrg)
		}

		d, err := s.GetDevice("dev-1")
		if err != nil {
			t.Fatalf("GetDevice: %v", err)
		}
		if d.TenantID != DefaultTenantID {
			t.Fatalf("device tenant after DeleteTenant = %q, want %q", d.TenantID, DefaultTenantID)
		}
		members, _ := s.ListTenantMembers("acme")
		if len(members) != 0 {
			t.Fatalf("expected no members left for a deleted tenant, got %v", members)
		}
		if _, ok, _ := s.GetTenantGrafanaOrg("acme"); ok {
			t.Fatal("expected the Grafana org mapping to be cleaned up")
		}
	})

	t.Run("ReassignDevice errors on an unknown tenant", func(t *testing.T) {
		s := newTestStore(t)
		must(t, s.EnsureDevice("dev-1"))
		if err := s.ReassignDevice("dev-1", "does-not-exist"); err == nil {
			t.Fatal("expected an error reassigning to an unknown tenant, got nil")
		}
	})

	t.Run("ReassignDevice errors on an unknown device", func(t *testing.T) {
		s := newTestStore(t)
		if err := s.ReassignDevice("does-not-exist", DefaultTenantID); err == nil {
			t.Fatal("expected an error reassigning an unknown device, got nil")
		}
	})
}

func TestTenantMembers(t *testing.T) {
	t.Run("AddTenantMember rejects an invalid role", func(t *testing.T) {
		s := newTestStore(t)
		if err := s.AddTenantMember(DefaultTenantID, "alice@example.com", "superuser"); err == nil {
			t.Fatal("expected an error for an invalid role, got nil")
		}
	})

	t.Run("AddTenantMember upserts the role on a repeat call", func(t *testing.T) {
		s := newTestStore(t)
		must(t, s.AddTenantMember(DefaultTenantID, "alice@example.com", RoleViewer))
		must(t, s.AddTenantMember(DefaultTenantID, "alice@example.com", RoleOperator))

		role, ok, err := s.RoleForLogin(DefaultTenantID, "alice@example.com")
		if err != nil {
			t.Fatalf("RoleForLogin: %v", err)
		}
		if !ok || role != RoleOperator {
			t.Fatalf("RoleForLogin = %q, %v; want %q, true", role, ok, RoleOperator)
		}
	})

	t.Run("RoleForLogin on a non-member returns ok=false", func(t *testing.T) {
		s := newTestStore(t)
		_, ok, err := s.RoleForLogin(DefaultTenantID, "nobody@example.com")
		if err != nil {
			t.Fatalf("RoleForLogin: %v", err)
		}
		if ok {
			t.Fatal("expected ok=false for a non-member")
		}
	})

	t.Run("a login can belong to more than one tenant", func(t *testing.T) {
		s := newTestStore(t)
		must(t, s.CreateTenant("t1", "T1"))
		must(t, s.CreateTenant("t2", "T2"))
		must(t, s.AddTenantMember("t1", "alice@example.com", RoleViewer))
		must(t, s.AddTenantMember("t2", "alice@example.com", RoleOperator))

		tenants, err := s.TenantsForLogin("alice@example.com")
		if err != nil {
			t.Fatalf("TenantsForLogin: %v", err)
		}
		if !equalStrings(sortedCopy(tenants), []string{"t1", "t2"}) {
			t.Fatalf("TenantsForLogin = %v, want [t1 t2]", tenants)
		}
	})

	t.Run("RemoveTenantMember", func(t *testing.T) {
		s := newTestStore(t)
		must(t, s.AddTenantMember(DefaultTenantID, "alice@example.com", RoleViewer))
		must(t, s.RemoveTenantMember(DefaultTenantID, "alice@example.com"))
		if _, ok, _ := s.RoleForLogin(DefaultTenantID, "alice@example.com"); ok {
			t.Fatal("expected alice to no longer be a member after RemoveTenantMember")
		}
	})

	t.Run("ListTenantMembers is sorted by login", func(t *testing.T) {
		s := newTestStore(t)
		must(t, s.AddTenantMember(DefaultTenantID, "zeta@example.com", RoleViewer))
		must(t, s.AddTenantMember(DefaultTenantID, "alpha@example.com", RoleOperator))

		members, err := s.ListTenantMembers(DefaultTenantID)
		if err != nil {
			t.Fatalf("ListTenantMembers: %v", err)
		}
		if len(members) != 2 || members[0].Login != "alpha@example.com" || members[1].Login != "zeta@example.com" {
			t.Fatalf("ListTenantMembers = %+v, want alpha before zeta", members)
		}
	})
}

func TestTenantGrafanaOrg(t *testing.T) {
	s := newTestStore(t)

	t.Run("unset returns ok=false", func(t *testing.T) {
		_, ok, err := s.GetTenantGrafanaOrg(DefaultTenantID)
		if err != nil {
			t.Fatalf("GetTenantGrafanaOrg: %v", err)
		}
		if ok {
			t.Fatal("expected ok=false before SetTenantGrafanaOrg")
		}
	})

	t.Run("set/get round-trip, and set again overwrites", func(t *testing.T) {
		must(t, s.SetTenantGrafanaOrg(DefaultTenantID, 7))
		orgID, ok, err := s.GetTenantGrafanaOrg(DefaultTenantID)
		if err != nil || !ok || orgID != 7 {
			t.Fatalf("GetTenantGrafanaOrg = %d, %v, %v; want 7, true, nil", orgID, ok, err)
		}

		must(t, s.SetTenantGrafanaOrg(DefaultTenantID, 8))
		orgID, _, _ = s.GetTenantGrafanaOrg(DefaultTenantID)
		if orgID != 8 {
			t.Fatalf("orgID after overwrite = %d, want 8", orgID)
		}
	})
}

func TestConversations(t *testing.T) {
	t.Run("CreateConversation/GetConversation ownership is enforced", func(t *testing.T) {
		s := newTestStore(t)
		id, err := s.CreateConversation(DefaultTenantID, "alice@example.com", "hello")
		if err != nil {
			t.Fatalf("CreateConversation: %v", err)
		}

		if _, err := s.GetConversation(id, DefaultTenantID, "alice@example.com"); err != nil {
			t.Fatalf("GetConversation(owner): %v", err)
		}
		if _, err := s.GetConversation(id, DefaultTenantID, "bob@example.com"); err == nil {
			t.Fatal("expected an error fetching another login's conversation, got nil")
		}
		if _, err := s.GetConversation(id, "other-tenant", "alice@example.com"); err == nil {
			t.Fatal("expected an error fetching a conversation under the wrong tenant, got nil")
		}
	})

	t.Run("ListConversations only returns the caller's own, most recently updated first", func(t *testing.T) {
		s := newTestStore(t)
		id1, err := s.CreateConversation(DefaultTenantID, "alice@example.com", "first")
		must(t, err)
		id2, err := s.CreateConversation(DefaultTenantID, "alice@example.com", "second")
		must(t, err)
		_, err = s.CreateConversation(DefaultTenantID, "bob@example.com", "not alice's")
		must(t, err)

		// Touch id1 so it becomes the most recently updated.
		must(t, s.AppendMessage(id1, "user", "hi", "", "", ""))

		convs, err := s.ListConversations(DefaultTenantID, "alice@example.com")
		if err != nil {
			t.Fatalf("ListConversations: %v", err)
		}
		if len(convs) != 2 {
			t.Fatalf("expected 2 conversations for alice, got %d: %+v", len(convs), convs)
		}
		if convs[0].ID != id1 {
			t.Fatalf("expected the touched conversation (id=%d) first, got %+v", id1, convs)
		}
		_ = id2
	})

	t.Run("DeleteConversation enforces ownership and cascades messages", func(t *testing.T) {
		s := newTestStore(t)
		id, err := s.CreateConversation(DefaultTenantID, "alice@example.com", "hello")
		must(t, err)
		must(t, s.AppendMessage(id, "user", "hi", "", "", ""))

		if err := s.DeleteConversation(id, DefaultTenantID, "bob@example.com"); err == nil {
			t.Fatal("expected an error deleting someone else's conversation, got nil")
		}
		if err := s.DeleteConversation(id, DefaultTenantID, "alice@example.com"); err != nil {
			t.Fatalf("DeleteConversation(owner): %v", err)
		}
		msgs, err := s.ListMessages(id)
		if err != nil {
			t.Fatalf("ListMessages after delete: %v", err)
		}
		if len(msgs) != 0 {
			t.Fatalf("expected messages to cascade-delete, got %v", msgs)
		}
	})

	t.Run("DeleteConversation on an unknown id errors", func(t *testing.T) {
		s := newTestStore(t)
		if err := s.DeleteConversation(999, DefaultTenantID, "alice@example.com"); err == nil {
			t.Fatal("expected an error deleting a nonexistent conversation, got nil")
		}
	})
}

func TestMessages(t *testing.T) {
	t.Run("AppendMessage preserves order and both message shapes", func(t *testing.T) {
		s := newTestStore(t)
		id, err := s.CreateConversation(DefaultTenantID, "alice@example.com", "hello")
		must(t, err)

		must(t, s.AppendMessage(id, "user", "what devices do I have?", "", "", ""))
		must(t, s.AppendMessage(id, "", "", "list_devices", `{}`, `{"devices":[]}`))
		must(t, s.AppendMessage(id, "assistant", "you have none", "", "", ""))

		msgs, err := s.ListMessages(id)
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		if len(msgs) != 3 {
			t.Fatalf("expected 3 messages, got %d", len(msgs))
		}
		if msgs[0].Content != "what devices do I have?" {
			t.Fatalf("msgs[0].Content = %q", msgs[0].Content)
		}
		if msgs[1].ToolName != "list_devices" || msgs[1].ToolResult != `{"devices":[]}` {
			t.Fatalf("msgs[1] = %+v, want a tool-call shape", msgs[1])
		}
		if msgs[2].Content != "you have none" {
			t.Fatalf("msgs[2].Content = %q", msgs[2].Content)
		}
	})

	t.Run("AppendMessage bumps the conversation's updated_at", func(t *testing.T) {
		s := newTestStore(t)
		id, err := s.CreateConversation(DefaultTenantID, "alice@example.com", "hello")
		must(t, err)
		before, err := s.GetConversation(id, DefaultTenantID, "alice@example.com")
		must(t, err)

		time.Sleep(1100 * time.Millisecond) // RFC3339 second-resolution timestamps
		must(t, s.AppendMessage(id, "user", "hi", "", "", ""))

		after, err := s.GetConversation(id, DefaultTenantID, "alice@example.com")
		must(t, err)
		if !after.UpdatedAt.After(before.UpdatedAt) {
			t.Fatalf("UpdatedAt didn't advance: before=%v after=%v", before.UpdatedAt, after.UpdatedAt)
		}
	})
}

func TestToolCallLog(t *testing.T) {
	s := newTestStore(t)

	must(t, s.LogToolCall(DefaultTenantID, "alice@example.com", "list_devices"))
	must(t, s.LogToolCall(DefaultTenantID, "alice@example.com", "get_device_details"))
	must(t, s.LogToolCall("other-tenant", "bob@example.com", "search_records"))

	t.Run("ListToolCallLog returns newest first, across tenants", func(t *testing.T) {
		entries, err := s.ListToolCallLog(10)
		if err != nil {
			t.Fatalf("ListToolCallLog: %v", err)
		}
		if len(entries) != 3 {
			t.Fatalf("expected 3 entries, got %d", len(entries))
		}
		if entries[0].ToolName != "search_records" {
			t.Fatalf("expected the most recent call first, got %+v", entries[0])
		}
	})

	t.Run("ListToolCallLog respects the limit", func(t *testing.T) {
		entries, err := s.ListToolCallLog(1)
		if err != nil {
			t.Fatalf("ListToolCallLog: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("expected exactly 1 entry, got %d", len(entries))
		}
	})
}

func TestSettings(t *testing.T) {
	t.Run("unset key returns ok=false", func(t *testing.T) {
		s := newTestStore(t)
		_, ok, err := s.GetSetting("nope")
		if err != nil {
			t.Fatalf("GetSetting: %v", err)
		}
		if ok {
			t.Fatal("expected ok=false for an unset key")
		}
	})

	t.Run("SetSetting/GetSetting round-trip and overwrite", func(t *testing.T) {
		s := newTestStore(t)
		must(t, s.SetSetting("k", "v1"))
		v, ok, err := s.GetSetting("k")
		if err != nil || !ok || v != "v1" {
			t.Fatalf("GetSetting = %q, %v, %v; want v1, true, nil", v, ok, err)
		}
		must(t, s.SetSetting("k", "v2"))
		v, _, _ = s.GetSetting("k")
		if v != "v2" {
			t.Fatalf("GetSetting after overwrite = %q, want v2", v)
		}
	})

	t.Run("GetSettingOr falls back only when unset", func(t *testing.T) {
		s := newTestStore(t)
		v, err := s.GetSettingOr("unset-key", "fallback")
		if err != nil || v != "fallback" {
			t.Fatalf("GetSettingOr(unset) = %q, %v; want fallback, nil", v, err)
		}
		must(t, s.SetSetting("set-key", "real-value"))
		v, err = s.GetSettingOr("set-key", "fallback")
		if err != nil || v != "real-value" {
			t.Fatalf("GetSettingOr(set) = %q, %v; want real-value, nil", v, err)
		}
	})

	t.Run("SetSettingIfAbsent never overwrites an existing value", func(t *testing.T) {
		s := newTestStore(t)
		must(t, s.SetSetting("k", "original"))
		must(t, s.SetSettingIfAbsent("k", "seed-value"))
		v, _, _ := s.GetSetting("k")
		if v != "original" {
			t.Fatalf("SetSettingIfAbsent overwrote an existing value: got %q, want %q", v, "original")
		}
	})

	t.Run("SetSettingIfAbsent sets the value when the key is new", func(t *testing.T) {
		s := newTestStore(t)
		must(t, s.SetSettingIfAbsent("new-key", "seed-value"))
		v, ok, _ := s.GetSetting("new-key")
		if !ok || v != "seed-value" {
			t.Fatalf("SetSettingIfAbsent(new key) = %q, %v; want seed-value, true", v, ok)
		}
	})

	t.Run("SetSettingIfAbsent treats an empty value as a no-op", func(t *testing.T) {
		s := newTestStore(t)
		must(t, s.SetSettingIfAbsent("k", ""))
		if _, ok, _ := s.GetSetting("k"); ok {
			t.Fatal("expected SetSettingIfAbsent(\"\") to not create a setting")
		}
	})
}

func TestMetricDescriptions(t *testing.T) {
	t.Run("empty by default", func(t *testing.T) {
		s := newTestStore(t)
		m, err := s.GetMetricDescriptions()
		if err != nil {
			t.Fatalf("GetMetricDescriptions: %v", err)
		}
		if len(m) != 0 {
			t.Fatalf("expected an empty map, got %v", m)
		}
	})

	t.Run("Set/Get/Remove round-trip", func(t *testing.T) {
		s := newTestStore(t)
		must(t, s.SetMetricDescription("valve_current", "Amperes drawn by the dosing valve"))
		must(t, s.SetMetricDescription("temperature", "Process temperature, Celsius"))

		m, err := s.GetMetricDescriptions()
		if err != nil {
			t.Fatalf("GetMetricDescriptions: %v", err)
		}
		if len(m) != 2 || m["valve_current"] == "" || m["temperature"] == "" {
			t.Fatalf("GetMetricDescriptions = %v, want both entries present", m)
		}

		must(t, s.RemoveMetricDescription("valve_current"))
		m, err = s.GetMetricDescriptions()
		if err != nil {
			t.Fatalf("GetMetricDescriptions after remove: %v", err)
		}
		if _, ok := m["valve_current"]; ok {
			t.Fatalf("expected valve_current to be gone, got %v", m)
		}
		if _, ok := m["temperature"]; !ok {
			t.Fatalf("expected temperature to survive removing a different key, got %v", m)
		}
	})

	t.Run("RemoveMetricDescription on an unknown name is a no-op, not an error", func(t *testing.T) {
		s := newTestStore(t)
		if err := s.RemoveMetricDescription("never-existed"); err != nil {
			t.Fatalf("RemoveMetricDescription(unknown): %v", err)
		}
	})
}

// --- helpers ---

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
