//go:build linux

// Package sysmetrics collects basic CPU/RAM/disk usage from /proc and the
// filesystem, without pulling in a general-purpose metrics library — the
// only target platform is Ubuntu.
package sysmetrics

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// cpuSample holds the raw counters from /proc/stat's aggregate "cpu" line.
type cpuSample struct {
	idle  uint64
	total uint64
}

func readCPUSample() (cpuSample, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuSample{}, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return cpuSample{}, fmt.Errorf("sysmetrics: empty /proc/stat")
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 8 || fields[0] != "cpu" {
		return cpuSample{}, fmt.Errorf("sysmetrics: unexpected /proc/stat format")
	}

	var sample cpuSample
	for _, f := range fields[1:] {
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return cpuSample{}, err
		}
		sample.total += v
	}
	// idle + iowait (fields[4], fields[5], 0-indexed from fields[1])
	idle, _ := strconv.ParseUint(fields[4], 10, 64)
	iowait, _ := strconv.ParseUint(fields[5], 10, 64)
	sample.idle = idle + iowait
	return sample, nil
}

// CPUPercent samples /proc/stat twice over the given interval and returns
// overall CPU utilization as a percentage (0-100).
func CPUPercent(ctx context.Context, interval time.Duration) (float64, error) {
	first, err := readCPUSample()
	if err != nil {
		return 0, err
	}

	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(interval):
	}

	second, err := readCPUSample()
	if err != nil {
		return 0, err
	}

	totalDelta := float64(second.total - first.total)
	idleDelta := float64(second.idle - first.idle)
	if totalDelta <= 0 {
		return 0, nil
	}
	return (1 - idleDelta/totalDelta) * 100, nil
}

// MemPercent returns used memory as a percentage (0-100), based on
// MemTotal/MemAvailable from /proc/meminfo.
func MemPercent() (float64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var total, available uint64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			total = parseMeminfoKB(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			available = parseMeminfoKB(line)
		}
	}
	if total == 0 {
		return 0, fmt.Errorf("sysmetrics: could not read MemTotal")
	}
	used := float64(total-available) / float64(total) * 100
	return used, nil
}

func parseMeminfoKB(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, _ := strconv.ParseUint(fields[1], 10, 64)
	return v
}

// DiskPercent returns used disk space as a percentage (0-100) for the
// filesystem containing path.
func DiskPercent(path string) (float64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	total := stat.Blocks * uint64(stat.Bsize)
	free := stat.Bfree * uint64(stat.Bsize)
	if total == 0 {
		return 0, fmt.Errorf("sysmetrics: zero-size filesystem at %s", path)
	}
	used := float64(total-free) / float64(total) * 100
	return used, nil
}
