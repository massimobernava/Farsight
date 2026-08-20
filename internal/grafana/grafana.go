// Package grafana provisions a Grafana Organization for a new tenant —
// see docs/MULTI-TENANCY.md Tappa 3a. Grafana's admin API only lets a
// server admin act on an org other than their own current one via a real
// (cookie-based) session switched with POST /api/user/using/:orgId — plain
// HTTP Basic Auth doesn't carry that state across requests, so this client
// logs in once and reuses the session cookie for the whole provisioning
// sequence.
package grafana

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
)

type Client struct {
	baseURL  string
	user     string
	password string
	http     *http.Client
}

func New(baseURL, user, password string) (*Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &Client{
		baseURL:  baseURL,
		user:     user,
		password: password,
		http:     &http.Client{Jar: jar},
	}, nil
}

func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("grafana %s %s: %s: %s", method, path, resp.Status, respBody)
	}
	return respBody, nil
}

// Login authenticates once and keeps the session cookie for every
// following call — required for SwitchOrg to actually stick (see package
// doc). Call this before anything else.
func (c *Client) Login(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodPost, "/login", map[string]string{
		"user":     c.user,
		"password": c.password,
	})
	return err
}

// CreateOrg creates a Grafana Organization and returns its ID. Fails if
// one with the same name already exists — callers should treat that as
// "already provisioned", not a hard error (see cmd/farsight-server).
func (c *Client) CreateOrg(ctx context.Context, name string) (int64, error) {
	respBody, err := c.do(ctx, http.MethodPost, "/api/orgs", map[string]string{"name": name})
	if err != nil {
		return 0, err
	}
	var out struct {
		OrgID int64 `json:"orgId"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return 0, err
	}
	return out.OrgID, nil
}

// DeleteOrg removes a Grafana Organization outright — used when a tenant
// itself is deleted (see docs/MULTI-TENANCY.md), best-effort like the
// rest of this package: a Grafana-side failure never blocks the tenant
// deletion on farsight-server's own side, which is the source of truth.
// Grafana refuses to delete the admin account's *current* org — same
// "current org is account state" gotcha as AddOrgUser — so the caller
// must SwitchOrg somewhere else first (see deleteGrafanaOrg in
// cmd/farsight-server, which switches to Org 1 before calling this).
func (c *Client) DeleteOrg(ctx context.Context, orgID int64) error {
	_, err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/api/orgs/%d", orgID), nil)
	return err
}

// SwitchOrg makes orgID the admin's active org for the rest of this
// client's session — every call after this (datasources, dashboards)
// applies there instead of the admin's default org.
func (c *Client) SwitchOrg(ctx context.Context, orgID int64) error {
	_, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/api/user/using/%d", orgID), nil)
	return err
}

// SetGrafanaAdmin grants or revokes Grafana Server Admin (global, not
// per-org — sees/manages every Organization) for userID. Used to cascade
// Farsight-admin status into Grafana: a Farsight admin already sees every
// tenant here, so it follows they should be able to manage Grafana itself
// too (see docs/MULTI-TENANCY.md admin panel design) — the reverse isn't
// implied, this is never called to revoke Farsight-admin.
func (c *Client) SetGrafanaAdmin(ctx context.Context, userID int64, isAdmin bool) error {
	path := fmt.Sprintf("/api/admin/users/%d/permissions", userID)
	_, err := c.do(ctx, http.MethodPut, path, map[string]any{"isGrafanaAdmin": isAdmin})
	return err
}

// CreateDatasource adds a datasource to whatever org SwitchOrg last set.
func (c *Client) CreateDatasource(ctx context.Context, spec map[string]any) error {
	_, err := c.do(ctx, http.MethodPost, "/api/datasources", spec)
	return err
}

// CreateDashboard adds/updates (by uid, if the model has one) a dashboard
// in whatever org SwitchOrg last set. model is the raw dashboard JSON
// model, same shape as GET /api/dashboards/uid/:uid's "dashboard" field.
func (c *Client) CreateDashboard(ctx context.Context, model map[string]any) error {
	_, err := c.do(ctx, http.MethodPost, "/api/dashboards/db", map[string]any{
		"dashboard": model,
		"overwrite": true,
	})
	return err
}

// EnsureUser returns the Grafana user id for login, creating a global
// account if none exists yet. The account never gets a usable password —
// login only ever happens via auth.proxy (see docs/MULTI-TENANCY.md
// "Opzione B"), so the random one set here is generated and discarded,
// never returned to any caller.
func (c *Client) EnsureUser(ctx context.Context, login string) (int64, error) {
	if id, err := c.lookupUser(ctx, login); err == nil {
		return id, nil
	}

	pwBytes := make([]byte, 20)
	if _, err := rand.Read(pwBytes); err != nil {
		return 0, err
	}
	respBody, err := c.do(ctx, http.MethodPost, "/api/admin/users", map[string]any{
		"login":    login,
		"email":    login,
		"password": hex.EncodeToString(pwBytes),
	})
	if err != nil {
		return 0, err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

func (c *Client) lookupUser(ctx context.Context, login string) (int64, error) {
	id, ok, err := c.LookupUserID(ctx, login)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("grafana: user %q not found", login)
	}
	return id, nil
}

// LookupUserID returns login's Grafana user id, or ok=false if no such
// account exists yet — unlike EnsureUser, this never creates one. Used to
// revoke Grafana Admin without the side effect of creating a phantom
// account just to immediately demote it (see RevokeGrafanaAdmin's caller
// in cmd/farsight-server).
func (c *Client) LookupUserID(ctx context.Context, login string) (id int64, ok bool, err error) {
	respBody, doErr := c.do(ctx, http.MethodGet, "/api/users/lookup?loginOrEmail="+login, nil)
	if doErr != nil {
		return 0, false, nil // 404 or any other failure: treat as "not found"
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return 0, false, err
	}
	if out.ID == 0 {
		return 0, false, nil
	}
	return out.ID, true, nil
}

// User is one row from ListUsers.
type User struct {
	ID             int64
	Login          string
	IsGrafanaAdmin bool
}

// ListUsers returns every Grafana user globally (Server Admin scope, not
// one Org) — used by the Farsight admin panel to show current Grafana
// Admin status for anyone Grafana already knows about, including someone
// made admin by hand before Farsight tracked this at all.
func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	respBody, err := c.do(ctx, http.MethodGet, "/api/users", nil)
	if err != nil {
		return nil, err
	}
	var out []struct {
		ID      int64  `json:"id"`
		Login   string `json:"login"`
		IsAdmin bool   `json:"isAdmin"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, err
	}
	users := make([]User, len(out))
	for i, u := range out {
		users[i] = User{ID: u.ID, Login: u.Login, IsGrafanaAdmin: u.IsAdmin}
	}
	return users, nil
}

// AddOrgUser adds an existing global Grafana user (userID, login — both
// needed: the add call takes a login, the update-role fallback takes an
// id) to orgID with role (e.g. "Viewer"). Idempotent: adding an
// already-member just updates their role instead.
func (c *Client) AddOrgUser(ctx context.Context, orgID, userID int64, login, role string) error {
	path := fmt.Sprintf("/api/orgs/%d/users", orgID)
	_, err := c.do(ctx, http.MethodPost, path, map[string]string{
		"loginOrEmail": login,
		"role":         role,
	})
	if err != nil {
		// Already a member: Grafana errors on POST for an existing
		// membership — PATCH the role instead rather than treating this
		// as a real failure.
		patchPath := fmt.Sprintf("/api/orgs/%d/users/%d", orgID, userID)
		_, patchErr := c.do(ctx, http.MethodPatch, patchPath, map[string]string{"role": role})
		if patchErr == nil {
			return nil
		}
		return err
	}
	return nil
}
