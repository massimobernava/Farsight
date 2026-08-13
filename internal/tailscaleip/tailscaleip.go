// Package tailscaleip resolves the machine's current Tailscale IPv4 address.
// The IP can change (re-auth, node key rotation), so callers should re-run
// Current() periodically rather than caching it for the process lifetime.
package tailscaleip

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Current returns the machine's current Tailscale IPv4 address by shelling
// out to the tailscale CLI. Returns an error if tailscaled is not running
// or the node is not authenticated (e.g. not yet joined the tailnet).
func Current(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "tailscale", "ip", "-4")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tailscale ip -4: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}

	ip := strings.TrimSpace(stdout.String())
	if ip == "" {
		return "", fmt.Errorf("tailscale ip -4: empty output")
	}
	// Multiple lines can be returned in edge cases (e.g. subnet routes);
	// the first line is always the node's own IPv4.
	if idx := strings.IndexByte(ip, '\n'); idx != -1 {
		ip = ip[:idx]
	}
	return ip, nil
}
