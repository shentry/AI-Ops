package grafana

import (
	"encoding/json"
	"errors"
	"testing"
)

// Both Grafana and the console resolve datasources by these fixed uids
// (deploy/monitoring/grafana/provisioning/datasources/datasources.yaml).
var provisionedDatasources = map[string]string{"prometheus": "prometheus", "loki": "loki"}

type ref struct {
	Type string `json:"type"`
	UID  string `json:"uid"`
}

type panel struct {
	ID         int    `json:"id"`
	Type       string `json:"type"`
	Title      string `json:"title"`
	Datasource *ref   `json:"datasource"`
	Targets    []struct {
		Expr       string `json:"expr"`
		Datasource *ref   `json:"datasource"`
	} `json:"targets"`
}

func TestDashboardsLoadInFileOrder(t *testing.T) {
	catalog, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	var uids []string
	for _, summary := range catalog.List() {
		uids = append(uids, summary.UID)
	}
	want := []string{"sub2api", "dependencies-host", "oncall-agent", "monitoring-stack"}
	if len(uids) != len(want) {
		t.Fatalf("uids = %v", uids)
	}
	for i := range want {
		if uids[i] != want[i] {
			t.Fatalf("uids = %v, want %v", uids, want)
		}
	}
	if _, err := catalog.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) error = %v", err)
	}
}

func TestDashboardsOnlyUseProvisionedDatasources(t *testing.T) {
	catalog, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, summary := range catalog.List() {
		body, _ := catalog.Get(summary.UID)
		var dashboard struct {
			Panels []panel `json:"panels"`
		}
		if err := json.Unmarshal(body, &dashboard); err != nil {
			t.Fatalf("%s: %v", summary.UID, err)
		}
		ids := map[int]bool{}
		for _, p := range dashboard.Panels {
			if ids[p.ID] {
				t.Fatalf("%s: duplicate panel id %d", summary.UID, p.ID)
			}
			ids[p.ID] = true
			if p.Type == "row" {
				continue
			}
			if p.Datasource == nil || provisionedDatasources[p.Datasource.Type] != p.Datasource.UID {
				t.Fatalf("%s/%s: datasource %+v is not provisioned", summary.UID, p.Title, p.Datasource)
			}
			if len(p.Targets) == 0 {
				t.Fatalf("%s/%s: no targets", summary.UID, p.Title)
			}
			for _, target := range p.Targets {
				// The console routes a panel by its datasource; a mixed panel
				// would send LogQL to Prometheus.
				if target.Expr == "" || target.Datasource == nil || *target.Datasource != *p.Datasource {
					t.Fatalf("%s/%s: target %+v does not match the panel datasource", summary.UID, p.Title, target)
				}
			}
		}
	}
}
