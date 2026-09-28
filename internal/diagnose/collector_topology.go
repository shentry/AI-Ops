package diagnose

import (
	"context"

	"oncall-agent/internal/topology"
)

// topologyCollector records the declared dependency graph with each node's live
// state and firing alerts. The structured snapshot is the whole content: the
// Guard reads it, and it renders as one facts line for the model.
type topologyCollector struct {
	topology *topology.Topology
}

func NewTopologyCollector(t *topology.Topology) Collector {
	return &topologyCollector{topology: t}
}

func (*topologyCollector) Name() string { return "topology" }

func (c *topologyCollector) Collect(ctx context.Context, _ Target) EvidenceItem {
	snapshot := c.topology.Snapshot(ctx)
	item := finishItem(c.Name(), "topology", "", nil)
	item.Topology = &snapshot
	return item
}
