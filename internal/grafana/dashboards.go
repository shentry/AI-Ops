// Package grafana holds the dashboard JSON shared by the monitoring stack and
// the console: Grafana provisions it from this directory
// (deploy/monitoring/compose.yaml), and the console's Monitor page renders the
// same panels natively. Layout follows ongrid, which embeds its dashboards
// under internal/manager/biz/grafana/dashboards.
package grafana

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"path"
)

//go:embed dashboards/*.json
var files embed.FS

// ErrNotFound is returned for an unknown dashboard uid.
var ErrNotFound = errors.New("grafana: dashboard not found")

// Summary identifies one dashboard for the console's board list.
type Summary struct {
	UID   string `json:"uid"`
	Title string `json:"title"`
}

// Dashboards is the parsed, immutable catalog keyed by uid.
type Dashboards struct {
	raw     map[string]json.RawMessage
	summary []Summary
}

// Load parses every embedded dashboard once at startup; a malformed or
// duplicate dashboard fails the boot instead of a page later. The numeric file
// name prefix fixes the order the console shows the boards in.
func Load() (*Dashboards, error) {
	entries, err := files.ReadDir("dashboards")
	if err != nil {
		return nil, fmt.Errorf("grafana: read dashboards: %w", err)
	}
	catalog := &Dashboards{raw: make(map[string]json.RawMessage, len(entries))}
	for _, entry := range entries {
		body, err := files.ReadFile(path.Join("dashboards", entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("grafana: read %s: %w", entry.Name(), err)
		}
		var head Summary
		if err := json.Unmarshal(body, &head); err != nil {
			return nil, fmt.Errorf("grafana: decode %s: %w", entry.Name(), err)
		}
		if head.UID == "" || head.Title == "" {
			return nil, fmt.Errorf("grafana: %s needs uid and title", entry.Name())
		}
		if _, dup := catalog.raw[head.UID]; dup {
			return nil, fmt.Errorf("grafana: duplicate dashboard uid %q", head.UID)
		}
		catalog.raw[head.UID] = body
		catalog.summary = append(catalog.summary, head)
	}
	return catalog, nil
}

// List returns the dashboards in file-name order.
func (d *Dashboards) List() []Summary {
	return append([]Summary(nil), d.summary...)
}

// Get returns one dashboard's JSON as provisioned into Grafana.
func (d *Dashboards) Get(uid string) (json.RawMessage, error) {
	body, ok := d.raw[uid]
	if !ok {
		return nil, ErrNotFound
	}
	return body, nil
}
