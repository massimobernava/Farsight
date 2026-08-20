// farsight-server is the control plane: it subscribes to every device's
// MQTT status/telemetry topics and serves a dashboard listing known
// machines with a direct link to each one's noVNC desktop. It never
// proxies VNC/SSH traffic itself.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/farsight/farsight/internal/config"
	"github.com/farsight/farsight/internal/dashboard"
	"github.com/farsight/farsight/internal/gallery"
	"github.com/farsight/farsight/internal/grafana"
	"github.com/farsight/farsight/internal/influxread"
	"github.com/farsight/farsight/internal/llmsettings"
	"github.com/farsight/farsight/internal/registry"
	"github.com/farsight/farsight/internal/store"
	"github.com/farsight/farsight/internal/tailscaleapi"
	"github.com/farsight/farsight/internal/tailscaleip"
	"github.com/farsight/farsight/internal/telemetry"
	"github.com/farsight/farsight/internal/tenants"
	"github.com/farsight/farsight/internal/webassets"
)

const defaultConfigPath = "/etc/farsight/server.conf"

func main() {
	if err := run(); err != nil {
		log.Fatalf("farsight-server: %v", err)
	}
}

func run() error {
	confPath := os.Getenv("FARSIGHT_SERVER_CONF")
	if confPath == "" {
		confPath = defaultConfigPath
	}
	cfg, err := config.ParseFile(confPath)
	if err != nil {
		return err
	}

	broker := cfg.Get("MQTT_BROKER", "tcp://127.0.0.1:1883")
	httpPort := cfg.Get("HTTP_PORT", "8080")
	websockifyPort := cfg.GetInt("WEBSOCKIFY_PORT", 6080)
	sshUser := cfg.Get("SSH_USER", "")
	bindIface := cfg.Get("BIND_INTERFACE", "tailscale")
	sqlitePath := cfg.Get("SQLITE_PATH", "/var/lib/farsight/farsight.db")
	uploadDir := cfg.Get("UPLOAD_DIR", "/var/lib/farsight/uploads")
	importersDir := cfg.Get("IMPORTERS_DIR", "/etc/farsight/importers")
	templatesDir := cfg.Get("TEMPLATES_DIR", "/etc/farsight/templates")
	reportsDir := cfg.Get("REPORTS_DIR", "/var/lib/farsight/reports")

	var bootstrapAdminLogins []string
	for _, l := range strings.Split(cfg.Get("ADMIN_TAILSCALE_LOGINS", ""), ",") {
		if l = strings.TrimSpace(l); l != "" {
			bootstrapAdminLogins = append(bootstrapAdminLogins, l)
		}
	}

	grafanaURL := cfg.Get("GRAFANA_URL", "")
	grafanaUser := cfg.Get("GRAFANA_ADMIN_USER", "admin")
	grafanaPassword := cfg.Get("GRAFANA_ADMIN_PASSWORD", "admin")
	influxTokenFile := cfg.Get("INFLUX_TOKEN_FILE", "/etc/farsight/influx_token")

	// LLM chat (see llmchat.go) has no server.conf equivalent at all — no
	// seed-from-file step for it, unlike most of this file's other
	// options: provider URL/API key/model/system prompt are configured
	// entirely on the /llm-settings admin page, live, no restart. A
	// fresh install just has the feature off (no client configured) until
	// an admin fills that page in.

	// Tailscale ACL sync (Tappa 3b) is optional — nil client means "not
	// configured," every sync call below just skips silently. Most
	// installs won't set these until they're ready to move access control
	// out of "everyone in the tailnet sees everything."
	var tsAPI *tailscaleapi.Client
	if tsClientID, tsClientSecret := cfg.Get("TAILSCALE_OAUTH_CLIENT_ID", ""), cfg.Get("TAILSCALE_OAUTH_CLIENT_SECRET", ""); tsClientID != "" && tsClientSecret != "" {
		tsAPI = tailscaleapi.New(tsClientID, tsClientSecret, cfg.Get("TAILSCALE_TAILNET", "-"))
	}

	st, err := store.Open(sqlitePath)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.BootstrapAdmins(bootstrapAdminLogins); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bindIP := "127.0.0.1"
	if bindIface == "tailscale" {
		ip, err := tailscaleip.Current(ctx)
		if err != nil {
			return err
		}
		bindIP = ip
	}
	if grafanaURL == "" {
		// Must be Grafana's real loopback-only address (see postinst,
		// Opzione B), NOT the Tailscale-IP:3000 farsight-grafana-proxy
		// fronts it with. That proxy identifies every caller via
		// `tailscale whois` and injects it as auth.proxy's trusted
		// header — which Grafana honors ahead of the Basic Auth
		// credentials below, so admin provisioning calls routed through
		// it would silently authenticate as whoever farsight-server's own
		// Tailscale identity resolves to instead of the real admin
		// account.
		grafanaURL = "http://127.0.0.1:3001"
	}

	// LLM tools (see cmd/farsight-server/llmtools.go) — optional like
	// tsAPI above: a missing/unreadable token file just means the
	// telemetry tools return "backend not configured" instead of data,
	// not a startup failure. Same InfluxDB instance/org/bucket Telegraf
	// writes to and provisionGrafanaOrg's datasource reads from.
	var influxClient *influxread.Client
	if tokenBytes, err := os.ReadFile(influxTokenFile); err != nil {
		log.Printf("influxread: could not read %s, LLM telemetry tools disabled: %v", influxTokenFile, err)
	} else {
		influxClient = influxread.New("http://127.0.0.1:8086", strings.TrimSpace(string(tokenBytes)), "farsight", influxBucket)
	}

	// Cascade Grafana Admin for every configured bootstrap admin too, not
	// just ones added later via the /admins UI (that handler already does
	// this on its own) — otherwise the very first admin, seeded straight
	// into the database by BootstrapAdmins above with no Grafana call
	// involved, would be the one login that doesn't get the cascade.
	// Idempotent (SetGrafanaAdmin true again is a no-op), so safe to run
	// on every boot regardless of whether this is a fresh bootstrap or
	// not. Backgrounded with retries, not inline: found live on zeus that
	// a package upgrade restarts grafana-server too (see postinst), and
	// Grafana takes several seconds longer than farsight-server to accept
	// connections again — a synchronous single attempt here raced it
	// ("connection refused") and would otherwise also delay MQTT
	// connect/HTTP listen startup waiting for a server that isn't even up
	// yet.
	go func() {
		for _, login := range bootstrapAdminLogins {
			if err := grantGrafanaAdminWithRetry(ctx, grafanaURL, grafanaUser, grafanaPassword, login); err != nil {
				log.Printf("grafana: failed to grant admin to bootstrap admin %q: %v", login, err)
			}
		}
	}()

	// Also run the Tailscale ACL sync once at boot, not just after the
	// first tenant/membership change — otherwise a fresh install with no
	// tenants yet wouldn't have its open "allow all" grant fixed (see
	// syncTailscaleACL) until *something* happened to trigger a sync,
	// which could be a while after install. Best-effort, backgrounded
	// like the cascade above: logs and moves on if it fails,
	// syncTailscaleACL runs again on every real tenant/membership event
	// regardless, so a failed attempt here isn't the only chance.
	if tsAPI != nil {
		go func() {
			if err := syncTailscaleACL(ctx, tsAPI, st); err != nil {
				log.Printf("tailscaleapi: acl sync failed at startup: %v", err)
			}
		}()
	}

	reg := registry.New()

	opts := mqtt.NewClientOptions().
		AddBroker(broker).
		SetClientID("farsight-server").
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetOnConnectHandler(func(c mqtt.Client) {
			log.Printf("connected to broker %s, subscribing", broker)
			c.Subscribe(telemetry.StatusWildcard, 1, onStatus(reg, st))
			c.Subscribe(telemetry.DataWildcard, 1, onData(reg, st))
			c.Subscribe(telemetry.AttributesWildcard, 1, onAttribute(reg, st))
			c.Subscribe(telemetry.RecordsWildcard, 1, onRecord(reg, st))
		})

	client := mqtt.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		return token.Error()
	}
	defer client.Disconnect(250)

	dashCfg := dashboard.Config{WebsockifyPort: websockifyPort, SSHUser: sshUser}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(webassets.FaviconICO)
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		id := identify(r.Context(), r, st)
		devices := filterForIdentity(listWithMeta(reg, st), id, st)
		// ?device=<device_id> filters to a single machine — used by the
		// Grafana "Macchina" dashboard's iframe panel, which interpolates
		// its own $device_id variable into this URL. Without the param,
		// every device shows (the "Sistema" dashboard's iframe panel).
		if filter := r.URL.Query().Get("device"); filter != "" {
			filtered := devices[:0]
			for _, d := range devices {
				if d.DeviceID == filter {
					filtered = append(filtered, d)
				}
			}
			devices = filtered
		}
		reqCfg := dashCfg
		reqCfg.CanAccess = accessChecker(r.Context(), st, id)
		reqCfg.IsAdmin = id.IsAdmin
		if id.IsAdmin {
			if tenantList, err := st.ListTenants(); err != nil {
				log.Printf("store: list tenants failed: %v", err)
			} else {
				for _, t := range tenantList {
					reqCfg.AllTenants = append(reqCfg.AllTenants, t.TenantID)
				}
			}
		}
		if err := dashboard.Render(w, devices, reqCfg); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("GET /api/devices", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id := identify(r.Context(), r, st)
		json.NewEncoder(w).Encode(filterForIdentity(listWithMeta(reg, st), id, st))
	})
	mux.HandleFunc("POST /devices/{device}/rename", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		device := r.PathValue("device")
		if err := st.SetDisplayName(device, r.FormValue("display_name")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	mux.HandleFunc("POST /devices/{device}/reassign", requireAdmin(st, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		device := r.PathValue("device")
		newTenant := r.FormValue("tenant_id")
		if err := st.ReassignDevice(device, newTenant); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Best-effort: the device's own row is the source of truth
		// regardless of whether this sync succeeds — see the ACL sync
		// pattern used everywhere else in this file.
		if err := syncTailscaleACL(r.Context(), tsAPI, st); err != nil {
			log.Printf("tailscaleapi: acl sync failed after reassigning %q to tenant %q: %v", device, newTenant, err)
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}))
	mux.HandleFunc("POST /devices/{device}/upload", uploadHandler(st, uploadDir, importersDir))
	mux.HandleFunc("GET /devices/{device}/files/{filename}", filesHandler(uploadDir))
	mux.HandleFunc("GET /records", recordsGalleryHandler(st, templatesDir))
	mux.HandleFunc("POST /templates/{name}", templateUploadHandler(templatesDir))
	mux.HandleFunc("GET /llm/reports/{tenant}/{filename}", llmReportsHandler(st, reportsDir))
	llmChat := llmChatHandler(llmChatDeps{
		tools: llmToolsDeps{
			st: st, reg: reg, influx: influxClient,
			uploadDir: uploadDir, reportsDir: reportsDir, bindIP: bindIP, httpPort: httpPort,
		},
		st: st,
	})
	mux.HandleFunc("POST /llm/chat", llmChat)
	mux.HandleFunc("OPTIONS /llm/chat", llmChat)
	myTenants := llmMyTenantsHandler(st)
	mux.HandleFunc("GET /llm/my-tenants", myTenants)
	mux.HandleFunc("OPTIONS /llm/my-tenants", myTenants)
	llmConversations := llmConversationsHandler(st)
	mux.HandleFunc("GET /llm/conversations", llmConversations)
	mux.HandleFunc("OPTIONS /llm/conversations", llmConversations)
	llmConversationMessages := llmConversationMessagesHandler(st)
	mux.HandleFunc("GET /llm/conversations/{id}/messages", llmConversationMessages)
	mux.HandleFunc("OPTIONS /llm/conversations/{id}/messages", llmConversationMessages)
	llmDeleteConversation := llmDeleteConversationHandler(st)
	mux.HandleFunc("POST /llm/conversations/{id}/delete", llmDeleteConversation)
	mux.HandleFunc("OPTIONS /llm/conversations/{id}/delete", llmDeleteConversation)

	mux.HandleFunc("GET /llm-settings", requireAdmin(st, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := renderLLMSettings(w, st); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	mux.HandleFunc("POST /llm-settings", requireAdmin(st, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := st.SetSetting(settingLLMProviderURL, strings.TrimRight(r.FormValue("provider_url"), "/")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := st.SetSetting(settingLLMModel, r.FormValue("model")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := st.SetSetting(settingLLMSystemPrompt, r.FormValue("system_prompt")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Blank means "leave unchanged" (see llmsettings' HasAPIKey copy)
		// — a secret field never round-trips its current value back into
		// the form, so an admin who isn't rotating it shouldn't
		// accidentally blank it out just by re-submitting the page.
		if key := r.FormValue("provider_api_key"); key != "" {
			if err := st.SetSetting(settingLLMProviderAPIKey, key); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		http.Redirect(w, r, "/llm-settings", http.StatusSeeOther)
	}))
	mux.HandleFunc("POST /llm-settings/metrics", requireAdmin(st, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		name := r.FormValue("name")
		if name == "" {
			http.Error(w, "missing name", http.StatusBadRequest)
			return
		}
		if err := st.SetMetricDescription(name, r.FormValue("description")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/llm-settings", http.StatusSeeOther)
	}))
	mux.HandleFunc("POST /llm-settings/metrics/{name}/remove", requireAdmin(st, func(w http.ResponseWriter, r *http.Request) {
		if err := st.RemoveMetricDescription(r.PathValue("name")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/llm-settings", http.StatusSeeOther)
	}))

	mux.HandleFunc("GET /tenants", requireAdmin(st, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		login := identify(r.Context(), r, st).Login
		if err := renderTenants(r.Context(), w, st, grafanaURL, grafanaUser, grafanaPassword, login); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	mux.HandleFunc("POST /admins", requireAdmin(st, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		login := r.FormValue("login")
		if login == "" {
			http.Error(w, "missing login", http.StatusBadRequest)
			return
		}
		if err := st.AddAdmin(login); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Cascade, best-effort: a Farsight admin gets Grafana Server Admin
		// too (see docs/MULTI-TENANCY.md) — farsight_admins is still the
		// source of truth for Farsight-admin status regardless of whether
		// this Grafana call succeeds, same pattern as every other
		// Grafana touch in this file.
		if err := grantGrafanaAdmin(r.Context(), grafanaURL, grafanaUser, grafanaPassword, login); err != nil {
			log.Printf("grafana: failed to grant admin to %q: %v", login, err)
		}
		http.Redirect(w, r, "/tenants", http.StatusSeeOther)
	}))
	mux.HandleFunc("POST /admins/{login}/remove", requireAdmin(st, func(w http.ResponseWriter, r *http.Request) {
		login := r.PathValue("login")
		if login == identify(r.Context(), r, st).Login {
			http.Error(w, "cannot remove yourself from Farsight admins", http.StatusBadRequest)
			return
		}
		if err := st.RemoveAdmin(login); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/tenants", http.StatusSeeOther)
	}))
	mux.HandleFunc("POST /grafana-admins", requireAdmin(st, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		login := r.FormValue("login")
		if login == "" {
			http.Error(w, "missing login", http.StatusBadRequest)
			return
		}
		// Not best-effort: unlike /admins, farsight_admins has no separate
		// record of Grafana-only admin status, so a failure here really
		// means nothing happened — surface it instead of pretending it
		// worked, unlike everywhere else Grafana is touched in this file.
		if err := grantGrafanaAdmin(r.Context(), grafanaURL, grafanaUser, grafanaPassword, login); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/tenants", http.StatusSeeOther)
	}))
	mux.HandleFunc("POST /grafana-admins/{login}/remove", requireAdmin(st, func(w http.ResponseWriter, r *http.Request) {
		login := r.PathValue("login")
		if err := revokeGrafanaAdmin(r.Context(), grafanaURL, grafanaUser, grafanaPassword, login); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/tenants", http.StatusSeeOther)
	}))
	mux.HandleFunc("POST /tenants", requireAdmin(st, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		tenantID, displayName := r.FormValue("tenant_id"), r.FormValue("display_name")
		if err := st.CreateTenant(tenantID, displayName); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Best-effort: a tenant is still created even if Grafana is
		// unreachable or misconfigured — this only enriches it with a
		// scoped Org, it's not the source of truth for the tenant itself.
		if err := provisionGrafanaOrg(r.Context(), st, grafanaURL, grafanaUser, grafanaPassword,
			influxTokenFile, bindIP, httpPort, tenantID, displayName); err != nil {
			log.Printf("grafana: org provisioning failed for tenant %q: %v", tenantID, err)
		}
		if err := syncTailscaleACL(r.Context(), tsAPI, st); err != nil {
			log.Printf("tailscaleapi: acl sync failed after creating tenant %q: %v", tenantID, err)
		}
		http.Redirect(w, r, "/tenants", http.StatusSeeOther)
	}))
	mux.HandleFunc("POST /tenants/{tenant}/delete", requireAdmin(st, func(w http.ResponseWriter, r *http.Request) {
		tenantID := r.PathValue("tenant")
		orgID, hadOrg, err := st.DeleteTenant(tenantID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if hadOrg {
			if err := deleteGrafanaOrg(r.Context(), grafanaURL, grafanaUser, grafanaPassword, orgID); err != nil {
				log.Printf("grafana: failed to delete org %d for tenant %q: %v", orgID, tenantID, err)
			}
		}
		if err := syncTailscaleACL(r.Context(), tsAPI, st); err != nil {
			log.Printf("tailscaleapi: acl sync failed after deleting tenant %q: %v", tenantID, err)
		}
		http.Redirect(w, r, "/tenants", http.StatusSeeOther)
	}))
	mux.HandleFunc("POST /tenants/{tenant}/members", requireAdmin(st, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		tenantID, login := r.PathValue("tenant"), r.FormValue("login")
		role := r.FormValue("role")
		if role == "" {
			role = store.RoleViewer
		}
		if err := st.AddTenantMember(tenantID, login, role); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Best-effort, same as tenant creation's Grafana provisioning:
		// membership in farsight-server's own tenant_members table is the
		// source of truth regardless of whether Grafana is reachable.
		if err := provisionGrafanaUser(r.Context(), st, grafanaURL, grafanaUser, grafanaPassword, tenantID, login, role); err != nil {
			log.Printf("grafana: user provisioning failed for %q in tenant %q: %v", login, tenantID, err)
		}
		if err := syncTailscaleACL(r.Context(), tsAPI, st); err != nil {
			log.Printf("tailscaleapi: acl sync failed after adding %q to tenant %q: %v", login, tenantID, err)
		}
		http.Redirect(w, r, "/tenants", http.StatusSeeOther)
	}))
	mux.HandleFunc("POST /tenants/{tenant}/members/{login}/remove", requireAdmin(st, func(w http.ResponseWriter, r *http.Request) {
		tenantID, login := r.PathValue("tenant"), r.PathValue("login")
		if err := st.RemoveTenantMember(tenantID, login); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Best-effort, same as everywhere else: farsight's own membership
		// table is updated regardless of whether Grafana/Tailscale are
		// reachable to reflect it — but a failure here means the actual
		// network/Grafana access this removal is supposed to revoke could
		// still be live until the next successful sync, worth calling
		// out clearly rather than only in a log line.
		if err := syncTailscaleACL(r.Context(), tsAPI, st); err != nil {
			log.Printf("tailscaleapi: acl sync failed after removing %q from tenant %q — Tailscale access may still be live until this succeeds: %v", login, tenantID, err)
		}
		http.Redirect(w, r, "/tenants", http.StatusSeeOther)
	}))

	addr := bindIP + ":" + httpPort
	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		<-ctx.Done()
		log.Println("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("farsight-server listening on http://%s (Tailscale-only)", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// identity is who's making a request, resolved via Tailscale — no login
// page, no password (see docs/MULTI-TENANCY.md). Login is empty if WhoIs
// fails (e.g. BIND_INTERFACE=loopback local debugging, no real tailnet
// connection to resolve) — treated as "no tenants, not admin," not an
// error page: fails closed rather than open.
type identity struct {
	Login   string
	IsAdmin bool
}

func identify(ctx context.Context, r *http.Request, st *store.Store) identity {
	login, err := tailscaleip.WhoIs(ctx, r.RemoteAddr)
	if err != nil {
		log.Printf("whois failed for %s: %v", r.RemoteAddr, err)
		return identity{}
	}
	isAdmin, err := st.IsAdmin(login)
	if err != nil {
		log.Printf("store: admin lookup failed for %q: %v", login, err)
	}
	return identity{Login: login, IsAdmin: isAdmin}
}

// filterForIdentity keeps only devices belonging to a tenant id is a
// member of — everything, if id is an admin.
func filterForIdentity(devices []registry.Device, id identity, st *store.Store) []registry.Device {
	if id.IsAdmin {
		return devices
	}
	tenantList, err := st.TenantsForLogin(id.Login)
	if err != nil {
		log.Printf("tenants for login %q failed: %v", id.Login, err)
		return nil
	}
	allowed := map[string]bool{}
	for _, t := range tenantList {
		allowed[t] = true
	}
	out := devices[:0]
	for _, d := range devices {
		if allowed[d.TenantID] {
			out = append(out, d)
		}
	}
	return out
}

// accessChecker returns a dashboard.Config.CanAccess function for id —
// admins see every VNC/SSH link, everyone else only on tenants where
// their own role is operator (see docs/MULTI-TENANCY.md "Sistema di
// ruoli/permessi": a viewer's role gives Grafana-only access, no
// VNC/SSH). One store lookup per distinct tenant id has already seen
// this request, not per device — fine at this scale (a handful of
// tenants a member belongs to, not device volume).
func accessChecker(ctx context.Context, st *store.Store, id identity) func(tenantID string) bool {
	if id.IsAdmin {
		return func(string) bool { return true }
	}
	cache := map[string]bool{}
	return func(tenantID string) bool {
		if can, ok := cache[tenantID]; ok {
			return can
		}
		role, ok, err := st.RoleForLogin(tenantID, id.Login)
		can := ok && role == store.RoleOperator
		if err != nil {
			log.Printf("role lookup failed for %q in tenant %q: %v", id.Login, tenantID, err)
			can = false
		}
		cache[tenantID] = can
		return can
	}
}

// requireAdmin gates a handler to logins in the farsight_admins table (see
// internal/store) — tenant creation/deletion/membership management, device
// reassignment, the admin panel itself, not device viewing (that's
// filterForIdentity's job, not an all-or-nothing gate).
func requireAdmin(st *store.Store, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !identify(r.Context(), r, st).IsAdmin {
			http.Error(w, "forbidden: admin only", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// renderTenants gathers every tenant plus its member list, and every
// Farsight admin, and renders the admin page — member lookups are one
// query per tenant, fine at the scale this page operates at (an admin's
// own tenant list, not device volume).
// renderLLMSettings gathers the current live LLM config (settings table
// + metric descriptions) and renders the admin page (see
// internal/llmsettings).
func renderLLMSettings(w io.Writer, st *store.Store) error {
	providerURL, err := st.GetSettingOr(settingLLMProviderURL, "")
	if err != nil {
		return err
	}
	model, err := st.GetSettingOr(settingLLMModel, "")
	if err != nil {
		return err
	}
	systemPrompt, err := st.GetSettingOr(settingLLMSystemPrompt, "")
	if err != nil {
		return err
	}
	apiKey, _, err := st.GetSetting(settingLLMProviderAPIKey)
	if err != nil {
		return err
	}
	descriptions, err := st.GetMetricDescriptions()
	if err != nil {
		return err
	}
	return llmsettings.Render(w, providerURL, model, systemPrompt, defaultSystemPrompt, apiKey != "", descriptions)
}

func renderTenants(ctx context.Context, w io.Writer, st *store.Store, grafanaURL, grafanaUser, grafanaPassword, currentLogin string) error {
	list, err := st.ListTenants()
	if err != nil {
		return err
	}
	members := map[string][]store.TenantMember{}
	for _, t := range list {
		m, err := st.ListTenantMembers(t.TenantID)
		if err != nil {
			return err
		}
		members[t.TenantID] = m
	}

	farsightAdmins, err := st.ListAdmins()
	if err != nil {
		return err
	}
	farsightAdminSet := map[string]bool{}
	knownLogins := map[string]bool{}
	for _, a := range farsightAdmins {
		farsightAdminSet[a] = true
		knownLogins[a] = true
	}
	for _, t := range list {
		for _, m := range members[t.TenantID] {
			knownLogins[m.Login] = true
		}
	}

	// Grafana admin status, best-effort: an unreachable Grafana just means
	// every "Grafana Admin" column shows false (and Grafana-only admins
	// Farsight hasn't otherwise seen won't show up in the table at all)
	// rather than a broken admin page — same best-effort spirit as every
	// other Grafana touch in this file.
	grafanaAdminSet := map[string]bool{}
	if gc, err := grafana.New(grafanaURL, grafanaUser, grafanaPassword); err != nil {
		log.Printf("grafana: client init failed while listing users: %v", err)
	} else if err := gc.Login(ctx); err != nil {
		log.Printf("grafana: login failed while listing users: %v", err)
	} else if grafanaUsers, err := gc.ListUsers(ctx); err != nil {
		log.Printf("grafana: list users failed: %v", err)
	} else {
		for _, u := range grafanaUsers {
			if u.Login == grafanaUser {
				continue // the bootstrap admin account itself, not a real person
			}
			knownLogins[u.Login] = true
			if u.IsGrafanaAdmin {
				grafanaAdminSet[u.Login] = true
			}
		}
	}

	logins := make([]string, 0, len(knownLogins))
	for l := range knownLogins {
		logins = append(logins, l)
	}
	sort.Strings(logins)

	users := make([]tenants.UserRow, len(logins))
	for i, l := range logins {
		users[i] = tenants.UserRow{Login: l, FarsightAdmin: farsightAdminSet[l], GrafanaAdmin: grafanaAdminSet[l]}
	}

	return tenants.Render(w, list, members, users, currentLogin)
}

// grantGrafanaAdmin gives login Grafana Server Admin (global) — creating
// the Grafana account first if it doesn't exist yet (see
// grafana.Client.EnsureUser: no need to wait for a first auth.proxy
// visit). Used both to cascade Farsight-admin status into Grafana (never
// the reverse) and as the independent "+ Grafana Admin" action — see
// docs/MULTI-TENANCY.md.
func grantGrafanaAdmin(ctx context.Context, grafanaURL, grafanaUser, grafanaPassword, login string) error {
	gc, err := grafana.New(grafanaURL, grafanaUser, grafanaPassword)
	if err != nil {
		return err
	}
	if err := gc.Login(ctx); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	userID, err := gc.EnsureUser(ctx, login)
	if err != nil {
		return fmt.Errorf("ensure user: %w", err)
	}
	return gc.SetGrafanaAdmin(ctx, userID, true)
}

// grantGrafanaAdminWithRetry retries grantGrafanaAdmin a few times, a
// couple seconds apart — for the one caller (the bootstrap-admin cascade
// at process startup) that has no natural retry the way a user clicking
// the UI again does, and can hit Grafana before it's finished its own
// restart (see the call site).
func grantGrafanaAdminWithRetry(ctx context.Context, grafanaURL, grafanaUser, grafanaPassword, login string) error {
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}
		if err = grantGrafanaAdmin(ctx, grafanaURL, grafanaUser, grafanaPassword, login); err == nil {
			return nil
		}
	}
	return err
}

// revokeGrafanaAdmin demotes login from Grafana Server Admin — a no-op if
// the account doesn't exist at all (nothing to revoke), unlike
// grantGrafanaAdmin this never creates one first.
func revokeGrafanaAdmin(ctx context.Context, grafanaURL, grafanaUser, grafanaPassword, login string) error {
	gc, err := grafana.New(grafanaURL, grafanaUser, grafanaPassword)
	if err != nil {
		return err
	}
	if err := gc.Login(ctx); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	userID, ok, err := gc.LookupUserID(ctx, login)
	if err != nil {
		return fmt.Errorf("lookup user: %w", err)
	}
	if !ok {
		return nil
	}
	return gc.SetGrafanaAdmin(ctx, userID, false)
}

// provisionGrafanaOrg gives a newly created tenant its own Grafana
// Organization: the same two datasources (InfluxDB, SQLite) every Org
// gets by default, and a home dashboard identical in spirit to the shared
// "Farsight - Sistema" one — an iframe onto farsight-server's own "/",
// which already filters by the viewer's Tailscale identity (Tappa 2), so
// this dashboard needs no tenant-specific query filtering of its own to
// only show that tenant's devices.
func provisionGrafanaOrg(ctx context.Context, st *store.Store, grafanaURL, grafanaUser, grafanaPassword,
	influxTokenFile, bindIP, httpPort, tenantID, displayName string) error {
	if _, ok, err := st.GetTenantGrafanaOrg(tenantID); err != nil {
		return err
	} else if ok {
		return nil // already provisioned, nothing to do
	}

	gc, err := grafana.New(grafanaURL, grafanaUser, grafanaPassword)
	if err != nil {
		return err
	}
	if err := gc.Login(ctx); err != nil {
		return fmt.Errorf("login: %w", err)
	}

	orgName := displayName
	if orgName == "" {
		orgName = tenantID
	}
	orgID, err := gc.CreateOrg(ctx, orgName)
	if err != nil {
		return fmt.Errorf("create org: %w", err)
	}
	if err := gc.SwitchOrg(ctx, orgID); err != nil {
		return fmt.Errorf("switch org: %w", err)
	}

	influxToken := ""
	if b, err := os.ReadFile(influxTokenFile); err == nil {
		influxToken = strings.TrimSpace(string(b))
	}
	if err := gc.CreateDatasource(ctx, map[string]any{
		"name":      "InfluxDB-Farsight",
		"type":      "influxdb",
		"access":    "proxy",
		"url":       "http://127.0.0.1:8086",
		"isDefault": true,
		"jsonData": map[string]any{
			"version":       "Flux",
			"organization":  "farsight",
			"defaultBucket": "telemetry",
		},
		"secureJsonData": map[string]any{"token": influxToken},
	}); err != nil {
		return fmt.Errorf("create influxdb datasource: %w", err)
	}
	if err := gc.CreateDatasource(ctx, map[string]any{
		"name":     "SQLite-Farsight",
		"type":     "frser-sqlite-datasource",
		"access":   "proxy",
		"jsonData": map[string]any{"path": "/var/lib/farsight/farsight.db"},
	}); err != nil {
		return fmt.Errorf("create sqlite datasource: %w", err)
	}

	dashboardURL := fmt.Sprintf("http://%s:%s/", bindIP, httpPort)
	if err := gc.CreateDashboard(ctx, map[string]any{
		"uid":           "farsight-" + tenantID,
		"title":         "Farsight - " + orgName,
		"tags":          []string{"farsight"},
		"timezone":      "browser",
		"schemaVersion": 39,
		"refresh":       "15s",
		"time":          map[string]any{"from": "now-6h", "to": "now"},
		"panels": []map[string]any{{
			"id":      1,
			"type":    "text",
			"title":   "Macchine",
			"gridPos": map[string]any{"h": 22, "w": 24, "x": 0, "y": 0},
			"options": map[string]any{
				"mode": "html",
				"content": fmt.Sprintf(
					`<iframe src="%s" style="width:100%%;height:100%%;border:0;background:white;"></iframe>`,
					dashboardURL),
			},
		}},
	}); err != nil {
		return fmt.Errorf("create dashboard: %w", err)
	}

	return st.SetTenantGrafanaOrg(tenantID, orgID)
}

// deleteGrafanaOrg removes tenant's Grafana Organization outright — used
// by POST /tenants/{tenant}/delete. Best-effort, same as everywhere else
// Grafana is touched from this file.
func deleteGrafanaOrg(ctx context.Context, grafanaURL, grafanaUser, grafanaPassword string, orgID int64) error {
	gc, err := grafana.New(grafanaURL, grafanaUser, grafanaPassword)
	if err != nil {
		return err
	}
	if err := gc.Login(ctx); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	// Grafana refuses to delete the admin account's *current* org ("cannot
	// delete your active organization") — found live while testing the
	// admin panel: a prior provisionGrafanaOrg/provisionGrafanaUser call
	// for this same tenant leaves the bootstrap admin switched into it.
	// Org 1 ("Main Org.") always exists, so switching there first is
	// always safe regardless of what the admin's current org happens to
	// be.
	if err := gc.SwitchOrg(ctx, 1); err != nil {
		return fmt.Errorf("switch to main org: %w", err)
	}
	return gc.DeleteOrg(ctx, orgID)
}

// grafanaRoleFor maps a tenant_members role to a Grafana org role — an
// operator gets Editor (can create/modify dashboards, including private
// ones — see docs/MULTI-TENANCY.md), a viewer gets Grafana's own Viewer.
func grafanaRoleFor(tenantRole string) string {
	if tenantRole == store.RoleOperator {
		return "Editor"
	}
	return "Viewer"
}

// provisionGrafanaUser gives login (already added to tenantID in
// tenant_members with role) a Grafana account — created eagerly here, not
// on first auth.proxy visit (auto_sign_up is off, see debian/postinst):
// the account only exists because farsight-server explicitly provisioned
// it, same tenant-membership gate as everything else, no separate signup
// path a non-member could hit.
func provisionGrafanaUser(ctx context.Context, st *store.Store, grafanaURL, grafanaUser, grafanaPassword, tenantID, login, role string) error {
	orgID, ok, err := st.GetTenantGrafanaOrg(tenantID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no Grafana org provisioned yet for tenant %q", tenantID)
	}

	gc, err := grafana.New(grafanaURL, grafanaUser, grafanaPassword)
	if err != nil {
		return err
	}
	if err := gc.Login(ctx); err != nil {
		return fmt.Errorf("login: %w", err)
	}

	userID, err := gc.EnsureUser(ctx, login)
	if err != nil {
		return fmt.Errorf("ensure user: %w", err)
	}
	// SwitchOrg is required here, not optional — POST /api/orgs/:id/users
	// checks the admin's *current* org context, which is whatever org this
	// same bootstrap admin account was last switched into (Grafana treats
	// "current org" as account state, not per-session — a previous,
	// unrelated provisioning call for a different tenant leaves it
	// pointed at that tenant's org).
	if err := gc.SwitchOrg(ctx, orgID); err != nil {
		return fmt.Errorf("switch org: %w", err)
	}
	if err := gc.AddOrgUser(ctx, orgID, userID, login, grafanaRoleFor(role)); err != nil {
		return fmt.Errorf("add org user: %w", err)
	}
	return nil
}

// rebuildGrants computes every tenant's Tailscale ACL grant from current
// state — tenant_members for who, DeviceIPsForTenant for which devices
// (looked up live from devices.tenant_id, so a reassignment is reflected
// immediately, no separate sync step needed there). Always a full
// rebuild, not an incremental patch: keeps the ACL's farsight-managed
// block trivially consistent with the database no matter which single
// thing changed to trigger it, and this runs rarely enough
// (tenant/membership/reassignment changes, not every telemetry message)
// that recomputing everything is cheap.
func rebuildGrants(st *store.Store) ([]tailscaleapi.Grant, error) {
	tenantList, err := st.ListTenants()
	if err != nil {
		return nil, err
	}
	var grants []tailscaleapi.Grant
	for _, t := range tenantList {
		members, err := st.ListTenantMembers(t.TenantID)
		if err != nil {
			return nil, err
		}
		ips, err := st.DeviceIPsForTenant(t.TenantID)
		if err != nil {
			return nil, err
		}
		if len(members) == 0 || len(ips) == 0 {
			continue // nothing to grant yet — no members, or no device has reported an IP
		}
		logins := make([]string, len(members))
		for i, m := range members {
			logins[i] = m.Login
		}
		grants = append(grants, tailscaleapi.Grant{Src: logins, Dst: ips})
	}
	return grants, nil
}

// syncTailscaleACL rewrites the farsight-managed block of the tailnet ACL
// policy from current state. Best-effort like the Grafana provisioning
// above: a nil client (TAILSCALE_OAUTH_CLIENT_ID/SECRET unset) is a
// silent no-op, not an error — this feature is opt-in.
func syncTailscaleACL(ctx context.Context, ts *tailscaleapi.Client, st *store.Store) error {
	if ts == nil {
		return nil
	}
	grants, err := rebuildGrants(st)
	if err != nil {
		return err
	}
	policy, etag, err := ts.GetACL(ctx)
	if err != nil {
		return fmt.Errorf("get acl: %w", err)
	}
	// Self-healing, every sync, not just once at install: a fresh
	// tailnet's default ACL ships with an open "allow all" grant, and as
	// long as it's there every tenant-scoped grant below is silently
	// redundant — anyone on the tailnet can already reach any device
	// directly by IP (see tailscaleapi.HasOpenAccessGrant doc). No point
	// waiting for an admin to notice and click something: it has to be
	// fixed eventually regardless, so fix it immediately, automatically,
	// same as the rest of this function's job.
	if tailscaleapi.HasOpenAccessGrant(policy) {
		fixedPolicy, fixed := tailscaleapi.FixOpenAccessGrant(policy)
		if fixed {
			log.Printf("tailscaleapi: tailnet ACL had an open \"allow all\" grant outside Farsight's managed block — replaced it with autogroup:self (own-devices-only) access")
			policy = fixedPolicy
		}
	}
	updated, err := tailscaleapi.UpsertGrantsBlock(policy, grants)
	if err != nil {
		return err
	}
	if err := ts.SetACL(ctx, updated, etag); err != nil {
		return fmt.Errorf("set acl: %w", err)
	}
	return nil
}

// listWithMeta merges the registry's live connectivity state with each
// device's persisted identity metadata, current tenant, and attributes
// from SQLite.
func listWithMeta(reg *registry.Registry, st *store.Store) []registry.Device {
	// Enumerates from the persistent store, not reg.List() — a device
	// imported once via file upload (see examples/generic-file-import),
	// with no continuously-running agent to re-touch the in-memory
	// registry, would otherwise vanish from the list entirely after the
	// next farsight-server restart clears it, even though it's still
	// fully known. Live status (online/last-seen) is merged in from the
	// registry where available, same pattern llmtools.go's
	// toolListDevices already used.
	all, err := st.ListAllDevices()
	if err != nil {
		log.Printf("store: list all devices failed: %v", err)
		return nil
	}

	live := map[string]registry.Device{}
	for _, d := range reg.List() {
		live[d.DeviceID] = d
	}

	devices := make([]registry.Device, len(all))
	for i, meta := range all {
		d := live[meta.DeviceID]
		devices[i] = registry.Device{
			DeviceID: meta.DeviceID, TenantID: meta.TenantID,
			DisplayName: meta.DisplayName, Notes: meta.Notes,
			Online: d.Online, LastSeen: d.LastSeen,
		}
		attrs, err := st.GetAttributes(meta.DeviceID)
		if err != nil {
			log.Printf("store: attributes lookup failed for %s: %v", meta.DeviceID, err)
			continue
		}
		devices[i].Attributes = attrs
	}
	return devices
}

const maxUploadBytes = 50 << 20 // 50MB

// uploadHandler accepts an arbitrary file from a device, stores it under
// uploadDir/<device>/, and — if one is configured — hands it off to an
// external importer. Deliberately format-agnostic: this is the generic
// "client pushes a file to the server" transport. farsight-server never
// parses file contents itself; it only knows how to look one script up by
// extension (importersDir/<ext, no dot>) and run it. No importer
// configured for an extension — including proprietary, one-off, "not an
// official feature" formats — means the file is simply saved and nothing
// else happens.
func uploadHandler(st *store.Store, uploadDir, importersDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		device := r.PathValue("device")

		// path.Base strips any directory components from the caller-supplied
		// name, so "../../etc/passwd" can't escape the per-device directory.
		filename := path.Base(r.URL.Query().Get("filename"))
		if filename == "" || filename == "." || filename == ".." || filename == "/" {
			http.Error(w, "missing or invalid ?filename=", http.StatusBadRequest)
			return
		}

		dir := filepath.Join(uploadDir, device)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// O_EXCL makes the name collision-proof outright, rather than just
		// unlikely: two uploads of the same filename landing in the same
		// nanosecond (same device, rapid/looped uploads — e.g. several
		// images for one treatment) would otherwise silently overwrite
		// each other instead of erroring or retrying.
		var f *os.File
		var dest, savedName string
		for attempt := 0; ; attempt++ {
			if attempt > 100 {
				http.Error(w, "could not allocate a unique filename", http.StatusInternalServerError)
				return
			}
			if attempt == 0 {
				savedName = fmt.Sprintf("%d-%s", time.Now().UnixNano(), filename)
			} else {
				savedName = fmt.Sprintf("%d-%d-%s", time.Now().UnixNano(), attempt, filename)
			}
			dest = filepath.Join(dir, savedName)
			var err error
			f, err = os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err == nil {
				break
			}
			if !os.IsExist(err) {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		defer f.Close()

		r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
		n, err := io.Copy(f, r.Body)
		if err != nil {
			os.Remove(dest)
			http.Error(w, "upload too large or connection interrupted", http.StatusBadRequest)
			return
		}

		if err := st.EnsureDevice(device); err != nil {
			log.Printf("store: ensure device failed for %s: %v", device, err)
		}

		log.Printf("upload: saved %d bytes to %s", n, dest)
		go runImporter(importersDir, dest, device)

		// Response body is just the saved filename (no path, no other
		// text) so callers — including the C SDK — can use it as-is, e.g.
		// as the record_id/reference for a farsight_publish_record call
		// linking this specific file to something (a treatment, ...).
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintln(w, savedName)
	}
}

// filesHandler serves back a file previously saved by uploadHandler — the
// read side of the same generic, format-agnostic mechanism. Needed because
// a browser (e.g. an <img> tag in a Grafana panel) can't read the server's
// local filesystem directly; this is the only way anything outside the
// server process gets the bytes back.
func filesHandler(uploadDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		device := r.PathValue("device")
		filename := path.Base(r.PathValue("filename")) // no path traversal out of the device's own directory
		if filename == "" || filename == "." || filename == ".." || filename == "/" {
			http.Error(w, "invalid filename", http.StatusBadRequest)
			return
		}
		filePath := filepath.Join(uploadDir, device, filename)

		if ct := mime.TypeByExtension(filepath.Ext(filename)); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		http.ServeFile(w, r, filePath)
	}
}

// recordsGalleryHandler renders every record matching ALL of the given
// ?filter.<field>=<value> query params (AND-ed, repeat for more — field
// can be any device_records column or any key inside a record's data,
// see store.FindRecordsByFilters) using the template named by
// ?template=<name> (default "default"). Everything about what shows and
// how is chosen by the caller — typically a Grafana iframe panel's URL,
// built from dashboard variables — with zero farsight-server code or
// config to touch for a new field, a new filter combination, or a new
// look. See docs/DEVELOPMENT.md.
func recordsGalleryHandler(st *store.Store, templatesDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filters := map[string]string{}
		for key, values := range r.URL.Query() {
			if field, ok := strings.CutPrefix(key, "filter."); ok && len(values) > 0 {
				filters[field] = values[0]
			}
		}
		if len(filters) == 0 {
			http.Error(w, "missing at least one ?filter.<field>=<value>", http.StatusBadRequest)
			return
		}

		records, err := st.FindRecordsByFilters(filters)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		templateName := path.Base(r.URL.Query().Get("template"))
		if templateName == "" || templateName == "." || templateName == ".." || templateName == "/" {
			templateName = "default"
		}
		templatePath := filepath.Join(templatesDir, templateName+".html.tmpl")

		// Re-loaded on every request on purpose: uploading/editing this
		// file is how the page's look is configured, and it should take
		// effect on the next reload, not need a farsight-server restart.
		t, err := gallery.LoadTemplate(templatePath)
		if err != nil {
			log.Printf("gallery: %s failed to load, using built-in default: %v", templatePath, err)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := gallery.Render(w, t, filters, records); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// templateUploadHandler saves the request body as
// templatesDir/<name>.html.tmpl, OVERWRITING any existing template with
// that name — unlike uploadHandler's uploads (which never collide, each
// gets a unique generated name), a template is a named, intentionally
// mutable resource: pushing "gallery" again means "update gallery," not
// "add another one." Same generic idea as the rest of the upload surface:
// from C, farsight_upload_template — no SSH/file access to the server
// needed to add or change a page's look.
func templateUploadHandler(templatesDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := path.Base(r.PathValue("name"))
		if name == "" || name == "." || name == ".." || name == "/" {
			http.Error(w, "invalid template name", http.StatusBadRequest)
			return
		}

		if err := os.MkdirAll(templatesDir, 0o755); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		dest := filepath.Join(templatesDir, name+".html.tmpl")

		f, err := os.Create(dest) // intentional overwrite
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer f.Close()

		r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
		n, err := io.Copy(f, r.Body)
		if err != nil {
			http.Error(w, "upload too large or connection interrupted", http.StatusBadRequest)
			return
		}

		log.Printf("template: saved %d bytes to %s", n, dest)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintln(w, name)
	}
}

// runImporter looks up importersDir/<ext> (extension without the leading
// dot, e.g. "dat" for a .dat file) and, if it exists and is executable,
// runs it as: importer <file-path> <device>. Whatever the script does
// with that — parse it, push data via MQTT, ignore it — is entirely up to
// whoever configured it; farsight-server just dispatches by extension and
// logs the outcome.
func runImporter(importersDir, filePath, device string) {
	ext := strings.TrimPrefix(filepath.Ext(filePath), ".")
	if ext == "" {
		return
	}
	importer := filepath.Join(importersDir, ext)
	if info, err := os.Stat(importer); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return // no importer configured for this extension, or not executable
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, importer, filePath, device)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("importer %s failed for %s: %v\n%s", importer, filePath, err, output)
		return
	}
	log.Printf("importer %s succeeded for %s\n%s", importer, filePath, output)
}

func onStatus(reg *registry.Registry, st *store.Store) mqtt.MessageHandler {
	return func(_ mqtt.Client, msg mqtt.Message) {
		deviceID, err := telemetry.ParseTopic(msg.Topic())
		if err != nil {
			log.Printf("status: %v", err)
			return
		}
		online := string(msg.Payload()) == telemetry.StatusOnline
		reg.SetStatus(deviceID, online)
		if err := st.EnsureDevice(deviceID); err != nil {
			log.Printf("store: ensure device failed for %s: %v", deviceID, err)
		}
	}
}

// onData only registers the device's existence — the telemetry itself
// (time-series metrics) is Telegraf's job to write to InfluxDB, not
// farsight-server's.
func onData(reg *registry.Registry, st *store.Store) mqtt.MessageHandler {
	return func(_ mqtt.Client, msg mqtt.Message) {
		var p telemetry.Payload
		if err := json.Unmarshal(msg.Payload(), &p); err != nil {
			log.Printf("telemetry: bad payload on %s: %v", msg.Topic(), err)
			return
		}
		reg.Touch(p.DeviceID)
		if err := st.EnsureDevice(p.DeviceID); err != nil {
			log.Printf("store: ensure device failed for %s: %v", p.DeviceID, err)
		}
	}
}

func onAttribute(reg *registry.Registry, st *store.Store) mqtt.MessageHandler {
	return func(_ mqtt.Client, msg mqtt.Message) {
		var a telemetry.Attribute
		if err := json.Unmarshal(msg.Payload(), &a); err != nil {
			log.Printf("attribute: bad payload on %s: %v", msg.Topic(), err)
			return
		}
		reg.Touch(a.DeviceID)
		if err := st.SetAttribute(a.DeviceID, a.Key, a.Value); err != nil {
			log.Printf("store: set attribute failed for %s: %v", a.DeviceID, err)
		}
	}
}

func onRecord(reg *registry.Registry, st *store.Store) mqtt.MessageHandler {
	return func(_ mqtt.Client, msg mqtt.Message) {
		var r telemetry.Record
		if err := json.Unmarshal(msg.Payload(), &r); err != nil {
			log.Printf("record: bad payload on %s: %v", msg.Topic(), err)
			return
		}
		reg.Touch(r.DeviceID)
		if err := st.SaveRecord(r.DeviceID, r.RecordID, r.Timestamp, r.Data); err != nil {
			log.Printf("store: save record failed for %s/%s: %v", r.DeviceID, r.RecordID, err)
		}
	}
}
