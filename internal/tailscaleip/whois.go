package tailscaleip

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// WhoIs returns the Tailscale login (email) of whoever owns remoteAddr (an
// "ip" or "ip:port" — an http.Request.RemoteAddr works directly). This is
// farsight-server's whole authentication mechanism: no login page, no
// password, because only tailnet members can reach this port at all, and
// tailscaled itself is the one authority on who's actually behind a given
// source address — see docs/MULTI-TENANCY.md.
func WhoIs(ctx context.Context, remoteAddr string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "tailscale", "whois", "--json", remoteAddr)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tailscale whois %s: %w (%s)", remoteAddr, err, strings.TrimSpace(stderr.String()))
	}

	var resp struct {
		UserProfile struct {
			LoginName string `json:"LoginName"`
		} `json:"UserProfile"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return "", fmt.Errorf("tailscale whois %s: bad json: %w", remoteAddr, err)
	}
	if resp.UserProfile.LoginName == "" {
		return "", fmt.Errorf("tailscale whois %s: no login name in response", remoteAddr)
	}
	return resp.UserProfile.LoginName, nil
}
