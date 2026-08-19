package ingest

import (
	"context"
	"errors"
	"strings"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/store"
)

// CorrelationInput is the already-normalized alert view used by D04.
// Identity fields are computed by the D03 worker and are not recomputed here.
type CorrelationInput struct {
	Fingerprint string
	Name        string
	Labels      map[string]string
	Severity    int
	ObservedAt  time.Time
}

type Assignment struct {
	IncidentID uint64
	Status     string
	Severity   int
	Created    bool
	Promoted   bool
}

type Correlator struct {
	cfg config.CorrelateConfig
}

const maxCorrelationWindowMinutes = (1<<63 - 1) / int64(time.Minute)

func correlationWindow(minutes int) (time.Duration, error) {
	if minutes <= 0 {
		return 0, errors.New("ingest: correlate window must be greater than zero")
	}
	if int64(minutes) > maxCorrelationWindowMinutes {
		return 0, errors.New("ingest: correlate window is too large")
	}
	return time.Duration(minutes) * time.Minute, nil
}

func NewCorrelator(cfg config.CorrelateConfig) *Correlator {
	return &Correlator{cfg: cfg}
}

// GroupKey returns a stable grouping key without hashing or depending on map order.
// A missing configured label falls back to the alert name so unrelated unnamed
// groups are not merged under a shared sentinel value.
func GroupKey(alert CorrelationInput, cfg config.CorrelateConfig) string {
	values := make([]string, 0, len(cfg.GroupBy))
	for _, field := range cfg.GroupBy {
		field = strings.TrimPrefix(strings.TrimSpace(field), "labels.")
		if field == "" {
			return "name:" + alert.Name
		}
		value, ok := alert.Labels[field]
		if !ok || strings.TrimSpace(value) == "" {
			return "name:" + alert.Name
		}
		values = append(values, value)
	}
	if len(values) == 0 {
		return "name:" + alert.Name
	}
	return strings.Join(values, ",")
}

func (c *Correlator) Assign(ctx context.Context, tx store.IncidentTx, alert CorrelationInput) (Assignment, error) {
	if c == nil {
		return Assignment{}, errors.New("ingest: correlator is nil")
	}
	if tx == nil {
		return Assignment{}, errors.New("ingest: incident transaction is nil")
	}
	window, err := correlationWindow(c.cfg.WindowMinutes)
	if err != nil {
		return Assignment{}, err
	}
	result, err := tx.AssignIncident(ctx, store.IncidentInput{
		GroupKey:    GroupKey(alert, c.cfg),
		Fingerprint: alert.Fingerprint,
		Name:        alert.Name,
		Severity:    alert.Severity,
		ObservedAt:  alert.ObservedAt,
	}, window, c.cfg.MinAlerts)
	if err != nil {
		return Assignment{}, err
	}
	return Assignment{
		IncidentID: result.IncidentID,
		Status:     result.Status,
		Severity:   result.Severity,
		Created:    result.Created,
		Promoted:   result.Promoted,
	}, nil
}
