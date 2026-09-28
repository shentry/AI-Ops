package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"

	"github.com/gogf/gf/v2/net/ghttp"

	"oncall-agent/internal/store"
	"oncall-agent/internal/topology"
)

// IncidentAlertReader is the one store read the topology page needs.
type IncidentAlertReader interface {
	ListIncidentAlerts(ctx context.Context, incidentID uint64) ([]store.Alert, error)
}

// TopologyAPI serves the topology page: the declared graph with live node
// states and, for ?incident=ID, the nodes that incident's alerts map to.
type TopologyAPI struct {
	topology *topology.Topology
	alerts   IncidentAlertReader
	auth     *Auth
}

func NewTopologyAPI(t *topology.Topology, alerts IncidentAlertReader, auth *Auth) *TopologyAPI {
	return &TopologyAPI{topology: t, alerts: alerts, auth: auth}
}

func (h *TopologyAPI) Handle(r *ghttp.Request) {
	h.ServeHTTP(r.Response.BufferWriter, r.Request)
}

type topologyResponse struct {
	topology.Snapshot
	Highlight []string `json:"highlight"`
}

func (h *TopologyAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.auth.Require(w, r, RoleViewer, false); !ok {
		return
	}
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}
	response := topologyResponse{Highlight: []string{}}
	if raw := r.URL.Query().Get("incident"); raw != "" {
		id, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || id == 0 {
			writeError(w, http.StatusBadRequest, "incident must be a positive integer")
			return
		}
		alerts, err := h.alerts.ListIncidentAlerts(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "incident alerts are unavailable")
			return
		}
		for _, alert := range alerts {
			var labels map[string]string
			if json.Unmarshal(alert.Labels, &labels) != nil {
				continue
			}
			if node := h.topology.NodeFor(labels); node != "" && !slices.Contains(response.Highlight, node) {
				response.Highlight = append(response.Highlight, node)
			}
		}
	}
	response.Snapshot = h.topology.Snapshot(r.Context())
	writeJSON(w, http.StatusOK, response)
}
