package diagnose

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"oncall-agent/internal/incident"
)

// VerificationObservation is a single read, not a recovery verdict. The worker
// applies the approved observation window before requesting any business effects.
type VerificationObservation struct {
	Observation string `json:"observation"`
	Detail      string `json:"detail"`
}

type healthResult struct {
	VerificationObservation
	statusCode int
	excerpt    string
	latencyMS  int64
}

// readHTTPHealth is the only /health implementation, shared by evidence and
// verification. Never follow redirects, send credentials, or expose URL-bearing
// transport errors. Response excerpts are only for evidence, not durable verdicts.
func readHTTPHealth(ctx context.Context, client *http.Client, baseURL string) healthResult {
	result := healthResult{VerificationObservation: VerificationObservation{Observation: "unavailable"}}
	base, err := incident.NormalizeHealthBaseURL(baseURL)
	if err != nil {
		result.Detail = "invalid health base URL"
		return result
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
	if err != nil {
		result.Detail = "cannot build health request"
		return result
	}
	// Copy the client so even a caller-provided client cannot enable redirects
	// or a credential-bearing cookie jar for this endpoint.
	boundedClient := *client
	boundedClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	boundedClient.Jar = nil
	started := time.Now()
	resp, err := boundedClient.Do(req)
	if err != nil {
		result.Detail = "health request unavailable or timed out"
		return result
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512))
	result.statusCode = resp.StatusCode
	result.latencyMS = time.Since(started).Milliseconds()
	result.excerpt = verifyText(string(body))
	if err != nil {
		result.Detail = "health response could not be read"
		return result
	}
	result.Detail = fmt.Sprintf("health returned HTTP %d", resp.StatusCode)
	result.Observation = "unhealthy"
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		result.Observation = "healthy"
	}
	return result
}

func verifyText(text string) string {
	text = Sanitize(ToSafeText(text))
	runes := []rune(text)
	if len(runes) <= 480 {
		return text
	}
	return string(runes[:480]) + "…[truncated]"
}
