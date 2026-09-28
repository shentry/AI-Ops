package sub2api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Probe sends one minimal real request through the sub2api gateway with a
// dedicated test key. Every call reaches an upstream model and costs money, so
// callers bound its frequency; the response body is discarded, never stored.
type Probe struct {
	url   string
	key   string
	body  []byte
	http  *http.Client
	clock func() time.Time
}

type ProbeResult struct {
	OK         bool
	StatusCode int
	Latency    time.Duration
	Detail     string
}

// NewProbe builds a probe for path (for example /v1/messages or
// /v1/chat/completions) that asks model for a single output token.
func NewProbe(baseURL, path, apiKey, model string, timeout time.Duration) (*Probe, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" {
		return nil, errors.New("sub2api: probe base URL must be an HTTP(S) origin")
	}
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#") || strings.TrimSpace(apiKey) == "" || strings.TrimSpace(model) == "" || timeout <= 0 {
		return nil, errors.New("sub2api: probe path, API key, model and timeout are required")
	}
	body, _ := json.Marshal(map[string]any{"model": model, "max_tokens": 1, "messages": []map[string]string{{"role": "user", "content": "ping"}}})
	return &Probe{url: strings.TrimRight(u.String(), "/") + path, key: apiKey, body: body,
		http:  &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		clock: time.Now}, nil
}

// Run performs one probe. Success requires HTTP 200 and a complete model response.
// Transport/read failures use status 0; response contents are never retained.
func (p *Probe) Run(ctx context.Context) ProbeResult {
	started := p.clock()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(p.body))
	if err != nil {
		return ProbeResult{Detail: "build probe request failed"}
	}
	req.Header.Set("Authorization", "Bearer "+p.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := p.http.Do(req)
	latency := p.clock().Sub(started)
	if err != nil {
		return ProbeResult{Latency: latency, Detail: "probe request failed: " + requestFailure(err)}
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	result := ProbeResult{StatusCode: resp.StatusCode, Latency: p.clock().Sub(started), Detail: fmt.Sprintf("status=%d", resp.StatusCode)}
	if readErr != nil || len(body) > 1<<20 {
		result.StatusCode, result.Detail = 0, "probe response incomplete or too large"
		return result
	}
	if resp.StatusCode != http.StatusOK {
		return result
	}
	var response struct {
		Error   json.RawMessage `json:"error"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &response) != nil || len(response.Error) > 0 && string(response.Error) != "null" {
		result.Detail = "probe returned an invalid business response"
		return result
	}
	for _, content := range response.Content {
		result.OK = result.OK || strings.TrimSpace(content.Text) != ""
	}
	for _, choice := range response.Choices {
		result.OK = result.OK || strings.TrimSpace(choice.Message.Content) != ""
	}
	if !result.OK {
		result.Detail = "probe returned no model output"
	}
	return result
}

func requestFailure(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return "timeout"
	}
	return "connection error"
}
