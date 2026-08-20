// Package svcstate checks whether a systemd unit is currently active.
package svcstate

import (
	"context"
	"os/exec"
	"time"
)

// IsActive reports whether the given systemd unit is active. It never
// returns an error: any failure (systemctl missing, unit not found, unit
// inactive) is reported simply as false, since callers only care about
// up/down for telemetry purposes.
func IsActive(ctx context.Context, unit string) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", unit)
	return cmd.Run() == nil
}
