package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
)

// BusinessTraffic is the single service's real SLA traffic in the last five minutes.
// Missing, stale or failed exporter reads never become zero traffic.
type BusinessTraffic struct {
	Requests  float64
	Errors    float64
	SampledAt time.Time
}

func (t BusinessTraffic) ErrorRatio() float64 {
	if t.Requests == 0 {
		return 0
	}
	return t.Errors / t.Requests
}

func ReadBusinessTraffic(ctx context.Context, registry *Registry) (BusinessTraffic, error) {
	var traffic BusinessTraffic
	evaluatedAt := time.Now().UTC()
	for _, query := range []struct {
		name string
		into *float64
	}{
		{`sub2api_requests_5m{class="sla"}`, &traffic.Requests},
		{`sub2api_errors_5m{class="sla"}`, &traffic.Errors},
	} {
		value, at, err := readCurrentScalar(ctx, registry, query.name, evaluatedAt)
		if err != nil {
			return BusinessTraffic{}, err
		}
		*query.into = value
		if traffic.SampledAt.IsZero() || at.Before(traffic.SampledAt) {
			traffic.SampledAt = at
		}
	}
	up, _, err := readCurrentScalar(ctx, registry, `sub2api_ops_up{endpoint="overview"}`, evaluatedAt)
	if err != nil {
		return BusinessTraffic{}, err
	}
	if up != 1 || traffic.Errors > traffic.Requests {
		return BusinessTraffic{}, errors.New("business metrics are unavailable or inconsistent")
	}
	return traffic, nil
}

func readCurrentScalar(ctx context.Context, registry *Registry, query string, evaluatedAt time.Time) (float64, time.Time, error) {
	// Instant-vector timestamps are evaluation times. timestamp(selector) reads
	// the underlying scrape time, so the server must filter stale samples before returning them.
	fresh := fmt.Sprintf(`(%s) and (timestamp(%s) >= time() - 120) and (timestamp(%s) <= time() + 1)`, query, query, query)
	args, _ := json.Marshal(map[string]string{"query": fresh, "time": evaluatedAt.Format(time.RFC3339Nano)})
	raw, meta, err := registry.ExecuteWithMetadata(ctx, ToolPromInstantQuery, args)
	if err != nil {
		return 0, time.Time{}, err
	}
	var data struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Value [2]json.RawMessage `json:"value"`
		} `json:"result"`
	}
	if meta.Truncated || json.Unmarshal([]byte(raw), &data) != nil || data.ResultType != "vector" || len(data.Result) != 1 {
		return 0, time.Time{}, fmt.Errorf("%s needs exactly one current sample", query)
	}
	var timestamp float64
	var text string
	if json.Unmarshal(data.Result[0].Value[0], &timestamp) != nil || json.Unmarshal(data.Result[0].Value[1], &text) != nil || math.IsNaN(timestamp) || math.IsInf(timestamp, 0) {
		return 0, time.Time{}, errors.New("invalid metric sample")
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, time.Time{}, errors.New("metric is not a finite non-negative value")
	}
	at := time.Unix(0, int64(timestamp*float64(time.Second))).UTC()
	age := time.Since(at)
	if age < -time.Second || age > 2*time.Minute {
		return 0, time.Time{}, errors.New("metric sample is stale or in the future")
	}
	return value, at, nil
}
