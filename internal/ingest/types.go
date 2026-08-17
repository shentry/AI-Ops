package ingest

import "time"

const (
	// SourceAlertmanager identifies normalized alerts received from Alertmanager.
	SourceAlertmanager = "alertmanager"

	// DefaultSeverityLabel is the label used by Alertmanager rules for severity.
	DefaultSeverityLabel = "severity"
)

// NormalizedAlert is the database-independent representation of one
// Alertmanager alert. ReceivedAt is assigned by the ingestion worker; the
// parser leaves it at the zero value so parsing remains deterministic.
type NormalizedAlert struct {
	Source       string
	Name         string
	Status       string
	Labels       map[string]string
	Annotations  map[string]string
	StartsAt     time.Time
	EndsAt       time.Time
	GeneratorURL string
	Severity     int
	Fingerprint  string
	AlertHash    string
	ReceivedAt   time.Time
}
