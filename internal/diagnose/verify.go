package diagnose

import (
	"context"
	"net/http"
	"time"

	"oncall-agent/internal/incident"
)

// Verifier performs one bounded, read-only health observation. It has no store,
// action tools, LLM, sleeping, or authority to declare a durable outcome.
type Verifier struct {
	binding    incident.ExecutionBinding
	httpClient *http.Client
}

func NewVerifier(binding incident.ExecutionBinding) *Verifier {
	return &Verifier{binding: binding, httpClient: &http.Client{}}
}

// Check uses the approved request timeout, capped by the remaining original
// window. A changed configuration never redirects an old approval to a new URL.
func (v *Verifier) Check(ctx context.Context, snapshot incident.ExecutionContext, remaining time.Duration) VerificationObservation {
	unavailable := func(detail string) VerificationObservation {
		return VerificationObservation{Observation: "unavailable", Detail: verifyText(detail)}
	}
	if err := snapshot.ValidateBinding(v.binding, false); err != nil {
		return unavailable(err.Error())
	}
	spec := snapshot.Verification
	if snapshot.DryRun || spec.Kind != incident.HealthVerification || spec.TimeoutSeconds <= 0 || spec.TimeoutSeconds >= 30 {
		return unavailable("approval has no supported real-action health check")
	}
	if ctx.Err() != nil || remaining <= 0 {
		return unavailable("health check canceled or verification window expired")
	}
	checkCtx, cancel := context.WithTimeout(ctx, min(time.Duration(spec.TimeoutSeconds)*time.Second, remaining))
	defer cancel()
	return readHTTPHealth(checkCtx, v.httpClient, spec.BaseURL).VerificationObservation
}
