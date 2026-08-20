// Package tailscaleapi talks to Tailscale's own cloud REST API (not the
// local tailscaled — see internal/tailscaleip for that) to read/write the
// tailnet's ACL policy — see docs/MULTI-TENANCY.md Tappa 3b. Scoped to
// exactly one thing: the "Policy File" OAuth permission, nothing else
// (no device tagging, no auth key minting — ACL grants reference
// Tailscale IPs directly instead, see UpsertGrantsBlock).
package tailscaleapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const apiBase = "https://api.tailscale.com/api/v2"

type Client struct {
	clientID     string
	clientSecret string
	tailnet      string
	http         *http.Client

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

// New creates a client for the given tailnet ("-" means "the tailnet this
// OAuth client belongs to", the normal case for a single-tailnet deploy).
func New(clientID, clientSecret, tailnet string) *Client {
	return &Client{
		clientID:     clientID,
		clientSecret: clientSecret,
		tailnet:      tailnet,
		http:         &http.Client{},
	}
}

// token returns a cached OAuth access token, or fetches a fresh one via
// the client-credentials flow if forceRefresh is set or none is cached
// yet. Tailscale appears to only honor one live access token per
// credential pair at a time — found by testing, not documented behavior
// we're relying on being permanent — so a long-lived process caching a
// token past its actual validity is a real failure mode, not just a
// theoretical one; authedRequest below retries once with forceRefresh on
// a 401 to recover from it.
func (c *Client) token(ctx context.Context, forceRefresh bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !forceRefresh && c.accessToken != "" && time.Now().Before(c.expiresAt.Add(-30*time.Second)) {
		return c.accessToken, nil
	}

	form := url.Values{
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.tailscale.com/api/v2/oauth/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("tailscaleapi: oauth token: %s: %s", resp.Status, body)
	}

	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	c.accessToken = out.AccessToken
	c.expiresAt = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	return c.accessToken, nil
}

// authedRequest builds and sends req (Authorization header added here —
// build must not add it), retrying once with a freshly fetched token if
// the first attempt comes back 401. build is called again on retry since
// an *http.Request's body can only be read once.
func (c *Client) authedRequest(ctx context.Context, build func(token string) (*http.Request, error)) (*http.Response, error) {
	token, err := c.token(ctx, false)
	if err != nil {
		return nil, err
	}
	req, err := build(token)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	resp.Body.Close()

	token, err = c.token(ctx, true)
	if err != nil {
		return nil, err
	}
	req, err = build(token)
	if err != nil {
		return nil, err
	}
	return c.http.Do(req)
}

// GetACL returns the tailnet's current ACL policy as raw text — HuJSON
// (JSON plus comments, Tailscale's own format for this file), not
// standard JSON. Callers must not round-trip this through encoding/json:
// that would silently drop every comment and reformat the whole file.
// See UpsertGrantsBlock for how this codebase edits it instead. etag is
// for SetACL's If-Match, to avoid clobbering a concurrent edit (e.g. made
// by hand in the Tailscale admin console) with a stale read.
func (c *Client) GetACL(ctx context.Context) (policy, etag string, err error) {
	resp, err := c.authedRequest(ctx, func(token string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("%s/tailnet/%s/acl", apiBase, c.tailnet), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return req, nil
	})
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("tailscaleapi: get acl: %s: %s", resp.Status, body)
	}
	return string(body), resp.Header.Get("ETag"), nil
}

// SetACL writes policy back — etag from the GetACL call this write is
// based on, sent as If-Match so Tailscale rejects the write outright if
// the policy changed underneath us instead of silently overwriting it.
func (c *Client) SetACL(ctx context.Context, policy, etag string) error {
	resp, err := c.authedRequest(ctx, func(token string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			fmt.Sprintf("%s/tailnet/%s/acl", apiBase, c.tailnet), bytes.NewReader([]byte(policy)))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/hujson")
		if etag != "" {
			req.Header.Set("If-Match", etag)
		}
		return req, nil
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("tailscaleapi: set acl: %s: %s", resp.Status, body)
	}
	return nil
}
