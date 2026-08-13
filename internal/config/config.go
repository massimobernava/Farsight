// Package config parses the simple KEY=VALUE files used by farsight
// binaries (/etc/farsight/client.conf, /etc/farsight/server.conf).
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Map is a parsed KEY=VALUE config file.
type Map map[string]string

// ParseFile reads a KEY=VALUE file, ignoring blank lines and lines starting
// with '#'. Values may be wrapped in single or double quotes.
func ParseFile(path string) (Map, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	m := Map{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		m[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return m, nil
}

// Get returns the value for key, or def if unset/empty.
func (m Map) Get(key, def string) string {
	if v, ok := m[key]; ok && v != "" {
		return v
	}
	return def
}

// GetInt returns the value for key parsed as int, or def if unset/invalid.
func (m Map) GetInt(key string, def int) int {
	v, ok := m[key]
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// Require returns the value for key, or an error if unset/empty.
func (m Map) Require(key string) (string, error) {
	v, ok := m[key]
	if !ok || v == "" {
		return "", fmt.Errorf("config: missing required key %q", key)
	}
	return v, nil
}
