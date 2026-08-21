package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/farsight/farsight/internal/registry"
	"github.com/farsight/farsight/internal/store"
)

// newTestDeps wires a real (temp-file) store and a real (empty) registry,
// like production — only deps.influx stays nil, since internal/influxread
// has no test double and talks to a real InfluxDB. Every tool path that
// needs influx is expected to fail fast with "telemetry backend not
// configured" once past its tenant-scoping check; that's exactly what's
// asserted below. Actual InfluxDB query behavior is out of scope for a
// unit test — see docs/BACKLOG.md.
func newTestDeps(t *testing.T) llmToolsDeps {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	return llmToolsDeps{
		st:         st,
		reg:        registry.New(),
		influx:     nil,
		uploadDir:  t.TempDir(),
		reportsDir: t.TempDir(),
		bindIP:     "100.1.2.3",
		httpPort:   "8080",
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

func TestCallTool(t *testing.T) {
	t.Run("unknown tool name errors", func(t *testing.T) {
		deps := newTestDeps(t)
		_, err := callTool(context.Background(), deps, store.DefaultTenantID, "alice@example.com", "does_not_exist", nil)
		if err == nil {
			t.Fatal("expected an error for an unknown tool, got nil")
		}
	})

	t.Run("a successful call is audit-logged", func(t *testing.T) {
		deps := newTestDeps(t)
		args := mustJSON(t, listDevicesArgs{})
		if _, err := callTool(context.Background(), deps, store.DefaultTenantID, "alice@example.com", "list_devices", args); err != nil {
			t.Fatalf("callTool: %v", err)
		}
		entries, err := deps.st.ListToolCallLog(10)
		if err != nil {
			t.Fatalf("ListToolCallLog: %v", err)
		}
		if len(entries) != 1 || entries[0].ToolName != "list_devices" || entries[0].Login != "alice@example.com" {
			t.Fatalf("audit log = %+v, want one list_devices entry for alice", entries)
		}
	})

	t.Run("a failed call is NOT audit-logged", func(t *testing.T) {
		deps := newTestDeps(t)
		args := mustJSON(t, getDeviceDetailsArgs{DeviceID: "does-not-exist"})
		if _, err := callTool(context.Background(), deps, store.DefaultTenantID, "alice@example.com", "get_device_details", args); err == nil {
			t.Fatal("expected an error for an unknown device, got nil")
		}
		entries, err := deps.st.ListToolCallLog(10)
		if err != nil {
			t.Fatalf("ListToolCallLog: %v", err)
		}
		if len(entries) != 0 {
			t.Fatalf("expected no audit entry for a failed call, got %+v", entries)
		}
	})
}

func TestDeviceInTenant(t *testing.T) {
	deps := newTestDeps(t)
	must(t, deps.st.CreateTenant("other", "Other"))
	must(t, deps.st.EnsureDevice("dev-mine"))
	must(t, deps.st.EnsureDevice("dev-other"))
	must(t, deps.st.ReassignDevice("dev-other", "other"))

	if _, err := deviceInTenant(deps.st, store.DefaultTenantID, "dev-mine"); err != nil {
		t.Fatalf("deviceInTenant(own device): %v", err)
	}
	if _, err := deviceInTenant(deps.st, store.DefaultTenantID, "dev-other"); err == nil {
		t.Fatal("expected an error for a device belonging to a different tenant, got nil")
	}
	if _, err := deviceInTenant(deps.st, store.DefaultTenantID, "never-seen"); err == nil {
		t.Fatal("expected an error for an unknown device, got nil")
	}
}

func TestToolListDevices(t *testing.T) {
	t.Run("only lists the caller's tenant, sorted", func(t *testing.T) {
		deps := newTestDeps(t)
		must(t, deps.st.CreateTenant("other", "Other"))
		must(t, deps.st.EnsureDevice("zebra"))
		must(t, deps.st.EnsureDevice("apple"))
		must(t, deps.st.EnsureDevice("in-other-tenant"))
		must(t, deps.st.ReassignDevice("in-other-tenant", "other"))

		out, err := toolListDevices(context.Background(), deps, store.DefaultTenantID, mustJSON(t, listDevicesArgs{}))
		if err != nil {
			t.Fatalf("toolListDevices: %v", err)
		}
		result := out.(map[string]any)
		if result["count"].(int) != 2 {
			t.Fatalf("count = %v, want 2", result["count"])
		}
		devices := result["devices"].([]deviceSummary)
		if len(devices) != 2 || devices[0].DeviceID != "apple" || devices[1].DeviceID != "zebra" {
			t.Fatalf("devices = %+v, want [apple zebra] sorted", devices)
		}
	})

	t.Run("online_only filters using live registry state", func(t *testing.T) {
		deps := newTestDeps(t)
		must(t, deps.st.EnsureDevice("online-dev"))
		must(t, deps.st.EnsureDevice("offline-dev"))
		deps.reg.SetStatus("online-dev", true)
		deps.reg.SetStatus("offline-dev", false)

		out, err := toolListDevices(context.Background(), deps, store.DefaultTenantID, mustJSON(t, listDevicesArgs{OnlineOnly: true}))
		if err != nil {
			t.Fatalf("toolListDevices: %v", err)
		}
		devices := out.(map[string]any)["devices"].([]deviceSummary)
		if len(devices) != 1 || devices[0].DeviceID != "online-dev" {
			t.Fatalf("devices = %+v, want only online-dev", devices)
		}
	})

	t.Run("empty args are valid (defaults to online_only=false)", func(t *testing.T) {
		deps := newTestDeps(t)
		must(t, deps.st.EnsureDevice("dev-1"))
		out, err := toolListDevices(context.Background(), deps, store.DefaultTenantID, nil)
		if err != nil {
			t.Fatalf("toolListDevices(nil args): %v", err)
		}
		if out.(map[string]any)["count"].(int) != 1 {
			t.Fatalf("expected 1 device with nil args, got %v", out)
		}
	})
}

func TestToolGetDeviceDetails(t *testing.T) {
	t.Run("rejects a device outside the caller's tenant", func(t *testing.T) {
		deps := newTestDeps(t)
		must(t, deps.st.CreateTenant("other", "Other"))
		must(t, deps.st.EnsureDevice("dev-1"))
		must(t, deps.st.ReassignDevice("dev-1", "other"))

		_, err := toolGetDeviceDetails(context.Background(), deps, store.DefaultTenantID, mustJSON(t, getDeviceDetailsArgs{DeviceID: "dev-1"}))
		if err == nil {
			t.Fatal("expected an error for a device in a different tenant, got nil")
		}
	})

	t.Run("returns attributes and live status; degrades gracefully with no influx client", func(t *testing.T) {
		deps := newTestDeps(t)
		must(t, deps.st.SetDisplayName("dev-1", "My Device"))
		must(t, deps.st.SetAttribute("dev-1", "firmware", "1.2.3"))
		deps.reg.SetStatus("dev-1", true)

		out, err := toolGetDeviceDetails(context.Background(), deps, store.DefaultTenantID, mustJSON(t, getDeviceDetailsArgs{DeviceID: "dev-1"}))
		if err != nil {
			t.Fatalf("toolGetDeviceDetails: %v", err)
		}
		result := out.(map[string]any)
		if result["display_name"] != "My Device" {
			t.Fatalf("display_name = %v, want %q", result["display_name"], "My Device")
		}
		if result["online"] != true {
			t.Fatalf("online = %v, want true", result["online"])
		}
		attrs := result["attributes"].(map[string]string)
		if attrs["firmware"] != "1.2.3" {
			t.Fatalf("attributes = %v", attrs)
		}
		if metrics := result["available_metrics"].([]metricInfo); len(metrics) != 0 {
			t.Fatalf("available_metrics = %v, want empty (no influx client configured)", metrics)
		}
	})
}

func TestResolveRange(t *testing.T) {
	t.Run("defaults to the last 24h when both are empty", func(t *testing.T) {
		from, to, err := resolveRange("", "")
		if err != nil {
			t.Fatalf("resolveRange: %v", err)
		}
		gotDur := to.Sub(from)
		if gotDur < 23*time.Hour || gotDur > 25*time.Hour {
			t.Fatalf("default range = %v, want ~24h", gotDur)
		}
	})

	t.Run("parses explicit RFC3339 bounds", func(t *testing.T) {
		from, to, err := resolveRange("2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z")
		if err != nil {
			t.Fatalf("resolveRange: %v", err)
		}
		if from.Format(time.RFC3339) != "2026-01-01T00:00:00Z" || to.Format(time.RFC3339) != "2026-01-02T00:00:00Z" {
			t.Fatalf("got from=%v to=%v", from, to)
		}
	})

	t.Run("rejects an invalid from", func(t *testing.T) {
		if _, _, err := resolveRange("not-a-date", ""); err == nil {
			t.Fatal("expected an error for an invalid from, got nil")
		}
	})

	t.Run("rejects an invalid to", func(t *testing.T) {
		if _, _, err := resolveRange("", "not-a-date"); err == nil {
			t.Fatal("expected an error for an invalid to, got nil")
		}
	})
}

func TestToolGetTelemetrySummary(t *testing.T) {
	t.Run("tenant scoping is checked before the influx client", func(t *testing.T) {
		deps := newTestDeps(t) // deps.influx is nil either way
		must(t, deps.st.CreateTenant("other", "Other"))
		must(t, deps.st.EnsureDevice("dev-1"))
		must(t, deps.st.ReassignDevice("dev-1", "other"))

		_, err := toolGetTelemetrySummary(context.Background(), deps, store.DefaultTenantID,
			mustJSON(t, telemetryRangeArgs{DeviceID: "dev-1", Metric: "temperature"}))
		if err == nil || strings.Contains(err.Error(), "telemetry backend") {
			t.Fatalf("expected a tenant-scoping error (not a telemetry-backend error), got %v", err)
		}
	})

	t.Run("errors clearly when no telemetry backend is configured", func(t *testing.T) {
		deps := newTestDeps(t)
		must(t, deps.st.EnsureDevice("dev-1"))

		_, err := toolGetTelemetrySummary(context.Background(), deps, store.DefaultTenantID,
			mustJSON(t, telemetryRangeArgs{DeviceID: "dev-1", Metric: "temperature"}))
		if err == nil || !strings.Contains(err.Error(), "telemetry backend not configured") {
			t.Fatalf("err = %v, want a telemetry-backend-not-configured error", err)
		}
	})
}

func TestToolGetTelemetrySeries(t *testing.T) {
	t.Run("tenant scoping is checked before the influx client", func(t *testing.T) {
		deps := newTestDeps(t)
		must(t, deps.st.CreateTenant("other", "Other"))
		must(t, deps.st.EnsureDevice("dev-1"))
		must(t, deps.st.ReassignDevice("dev-1", "other"))

		_, err := toolGetTelemetrySeries(context.Background(), deps, store.DefaultTenantID,
			mustJSON(t, telemetrySeriesArgs{telemetryRangeArgs: telemetryRangeArgs{DeviceID: "dev-1", Metric: "temperature"}}))
		if err == nil || strings.Contains(err.Error(), "telemetry backend") {
			t.Fatalf("expected a tenant-scoping error (not a telemetry-backend error), got %v", err)
		}
	})

	t.Run("errors clearly when no telemetry backend is configured", func(t *testing.T) {
		deps := newTestDeps(t)
		must(t, deps.st.EnsureDevice("dev-1"))

		_, err := toolGetTelemetrySeries(context.Background(), deps, store.DefaultTenantID,
			mustJSON(t, telemetrySeriesArgs{telemetryRangeArgs: telemetryRangeArgs{DeviceID: "dev-1", Metric: "temperature"}}))
		if err == nil || !strings.Contains(err.Error(), "telemetry backend not configured") {
			t.Fatalf("err = %v, want a telemetry-backend-not-configured error", err)
		}
	})
}

func TestToolSearchRecords(t *testing.T) {
	t.Run("rejects empty filters", func(t *testing.T) {
		deps := newTestDeps(t)
		_, err := toolSearchRecords(context.Background(), deps, store.DefaultTenantID, mustJSON(t, searchRecordsArgs{}))
		if err == nil {
			t.Fatal("expected an error for empty filters, got nil")
		}
	})

	t.Run("never returns a record belonging to a device outside the caller's tenant", func(t *testing.T) {
		deps := newTestDeps(t)
		must(t, deps.st.CreateTenant("other", "Other"))
		must(t, deps.st.EnsureDevice("mine"))
		must(t, deps.st.EnsureDevice("theirs"))
		must(t, deps.st.ReassignDevice("theirs", "other"))
		must(t, deps.st.SaveRecord("mine", "rec-1", time.Now(), map[string]string{"kind": "batch"}))
		must(t, deps.st.SaveRecord("theirs", "rec-2", time.Now(), map[string]string{"kind": "batch"}))

		out, err := toolSearchRecords(context.Background(), deps, store.DefaultTenantID,
			mustJSON(t, searchRecordsArgs{Filters: map[string]string{"kind": "batch"}}))
		if err != nil {
			t.Fatalf("toolSearchRecords: %v", err)
		}
		result := out.(map[string]any)
		records := result["records"].([]store.RecordMeta)
		if len(records) != 1 || records[0].DeviceID != "mine" {
			t.Fatalf("records = %+v, want only the caller's own tenant's record", records)
		}
	})

	t.Run("truncates to the limit and reports total_available", func(t *testing.T) {
		deps := newTestDeps(t)
		must(t, deps.st.EnsureDevice("dev-1"))
		for i := 0; i < 5; i++ {
			must(t, deps.st.SaveRecord("dev-1", "rec-"+string(rune('a'+i)), time.Now(), map[string]string{"kind": "x"}))
		}

		out, err := toolSearchRecords(context.Background(), deps, store.DefaultTenantID,
			mustJSON(t, searchRecordsArgs{Filters: map[string]string{"kind": "x"}, Limit: 2}))
		if err != nil {
			t.Fatalf("toolSearchRecords: %v", err)
		}
		result := out.(map[string]any)
		if result["count"].(int) != 2 {
			t.Fatalf("count = %v, want 2", result["count"])
		}
		if result["truncated"] != true {
			t.Fatalf("truncated = %v, want true", result["truncated"])
		}
		if result["total_available"].(int) != 5 {
			t.Fatalf("total_available = %v, want 5", result["total_available"])
		}
	})
}

func TestToolGenerateReport(t *testing.T) {
	t.Run("rejects empty content", func(t *testing.T) {
		deps := newTestDeps(t)
		_, err := toolGenerateReport(context.Background(), deps, store.DefaultTenantID,
			mustJSON(t, generateReportArgs{Title: "Empty"}))
		if err == nil {
			t.Fatal("expected an error for empty content_markdown, got nil")
		}
	})

	t.Run("writes the file under reportsDir/tenantID and returns a matching download URL", func(t *testing.T) {
		deps := newTestDeps(t)
		out, err := toolGenerateReport(context.Background(), deps, store.DefaultTenantID,
			mustJSON(t, generateReportArgs{Title: "Weekly summary", ContentMarkdown: "# Report\n\nAll good."}))
		if err != nil {
			t.Fatalf("toolGenerateReport: %v", err)
		}
		result := out.(map[string]any)
		reportID := result["report_id"].(string)
		wantURL := "http://100.1.2.3:8080/llm/reports/" + store.DefaultTenantID + "/" + reportID + ".md"
		if result["download_url"] != wantURL {
			t.Fatalf("download_url = %v, want %v", result["download_url"], wantURL)
		}

		content, err := os.ReadFile(filepath.Join(deps.reportsDir, store.DefaultTenantID, reportID+".md"))
		if err != nil {
			t.Fatalf("report file not written where expected: %v", err)
		}
		if string(content) != "# Report\n\nAll good." {
			t.Fatalf("report content = %q", content)
		}
	})
}

func TestToolGetFileContent(t *testing.T) {
	t.Run("rejects a device outside the caller's tenant", func(t *testing.T) {
		deps := newTestDeps(t)
		must(t, deps.st.CreateTenant("other", "Other"))
		must(t, deps.st.EnsureDevice("dev-1"))
		must(t, deps.st.ReassignDevice("dev-1", "other"))

		_, err := toolGetFileContent(context.Background(), deps, store.DefaultTenantID,
			mustJSON(t, getFileContentArgs{DeviceID: "dev-1", Filename: "a.png"}))
		if err == nil {
			t.Fatal("expected an error for a device in a different tenant, got nil")
		}
	})

	t.Run("path traversal in the filename is sandboxed to the device's own directory", func(t *testing.T) {
		deps := newTestDeps(t)
		must(t, deps.st.EnsureDevice("dev-1"))
		// A file that really exists, but only reachable by its base name —
		// "../../../etc/passwd" must resolve to looking for a file literally
		// named "passwd" inside dev-1's own upload directory, not escape it.
		must(t, os.MkdirAll(filepath.Join(deps.uploadDir, "dev-1"), 0o755))
		must(t, os.WriteFile(filepath.Join(deps.uploadDir, "dev-1", "passwd"), []byte("not actually /etc/passwd"), 0o644))

		out, err := toolGetFileContent(context.Background(), deps, store.DefaultTenantID,
			mustJSON(t, getFileContentArgs{DeviceID: "dev-1", Filename: "../../../etc/passwd"}))
		if err != nil {
			t.Fatalf("toolGetFileContent: %v", err)
		}
		result := out.(map[string]any)
		if result["filename"] != "passwd" {
			t.Fatalf("filename = %v, want the sandboxed base name %q", result["filename"], "passwd")
		}
		decoded, _ := base64.StdEncoding.DecodeString(result["data_base64"].(string))
		if string(decoded) != "not actually /etc/passwd" {
			t.Fatalf("decoded content = %q", decoded)
		}
	})

	t.Run("rejects a filename that resolves to empty/dot/dotdot", func(t *testing.T) {
		deps := newTestDeps(t)
		must(t, deps.st.EnsureDevice("dev-1"))
		for _, bad := range []string{"", ".", "..", "/"} {
			_, err := toolGetFileContent(context.Background(), deps, store.DefaultTenantID,
				mustJSON(t, getFileContentArgs{DeviceID: "dev-1", Filename: bad}))
			if err == nil {
				t.Fatalf("filename %q: expected an error, got nil", bad)
			}
		}
	})

	t.Run("missing file errors", func(t *testing.T) {
		deps := newTestDeps(t)
		must(t, deps.st.EnsureDevice("dev-1"))
		_, err := toolGetFileContent(context.Background(), deps, store.DefaultTenantID,
			mustJSON(t, getFileContentArgs{DeviceID: "dev-1", Filename: "never-uploaded.png"}))
		if err == nil {
			t.Fatal("expected an error for a missing file, got nil")
		}
	})

	t.Run("a file over the size cap is rejected, not handed to the model", func(t *testing.T) {
		deps := newTestDeps(t)
		must(t, deps.st.EnsureDevice("dev-1"))
		must(t, os.MkdirAll(filepath.Join(deps.uploadDir, "dev-1"), 0o755))
		big := make([]byte, maxToolFileBytes+1)
		must(t, os.WriteFile(filepath.Join(deps.uploadDir, "dev-1", "huge.bin"), big, 0o644))

		_, err := toolGetFileContent(context.Background(), deps, store.DefaultTenantID,
			mustJSON(t, getFileContentArgs{DeviceID: "dev-1", Filename: "huge.bin"}))
		if err == nil {
			t.Fatal("expected an error for a file over maxToolFileBytes, got nil")
		}
	})

	t.Run("valid read returns base64 content and a MIME type", func(t *testing.T) {
		deps := newTestDeps(t)
		must(t, deps.st.EnsureDevice("dev-1"))
		must(t, os.MkdirAll(filepath.Join(deps.uploadDir, "dev-1"), 0o755))
		must(t, os.WriteFile(filepath.Join(deps.uploadDir, "dev-1", "note.txt"), []byte("hello"), 0o644))

		out, err := toolGetFileContent(context.Background(), deps, store.DefaultTenantID,
			mustJSON(t, getFileContentArgs{DeviceID: "dev-1", Filename: "note.txt"}))
		if err != nil {
			t.Fatalf("toolGetFileContent: %v", err)
		}
		result := out.(map[string]any)
		decoded, _ := base64.StdEncoding.DecodeString(result["data_base64"].(string))
		if string(decoded) != "hello" {
			t.Fatalf("decoded content = %q, want %q", decoded, "hello")
		}
		if result["mime_type"] == "" {
			t.Fatal("expected a non-empty mime_type")
		}
		if result["size_bytes"] != int64(5) {
			t.Fatalf("size_bytes = %v, want 5", result["size_bytes"])
		}
	})
}
