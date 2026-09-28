// Package sub2api is the only client of the sub2api admin API. The admin key
// has full authority and no read-only scope, so every caller is built with the
// exact endpoints it may call and anything else is refused before any I/O.
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
	"strconv"
	"strings"
	"time"
)

// Endpoint is one admin route relative to /api/v1/admin. {id} is the only
// path parameter and is always an integer supplied by typed methods.
type Endpoint struct {
	Method string
	Path   string
}

var (
	OpsOverview       = Endpoint{http.MethodGet, "/ops/dashboard/overview"}
	OpsAvailability   = Endpoint{http.MethodGet, "/ops/account-availability"}
	OpsUpstreamErrors = Endpoint{http.MethodGet, "/ops/upstream-errors"}
	GetAccount        = Endpoint{http.MethodGet, "/accounts/{id}"}
	SetSchedulable    = Endpoint{http.MethodPost, "/accounts/{id}/schedulable"}
)

// ReadOnlyOps is the allowlist of the exporter and evidence collectors.
var ReadOnlyOps = []Endpoint{OpsOverview, OpsAvailability, OpsUpstreamErrors, GetAccount}

const maxResponseBytes = 4 << 20

// Client calls only the endpoints it was built with.
type Client struct {
	base    string
	key     string
	http    *http.Client
	allowed map[Endpoint]bool
}

func New(baseURL, apiKey string, timeout time.Duration, allowed ...Endpoint) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" {
		return nil, errors.New("sub2api: base URL must be an HTTP(S) origin")
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("sub2api: admin API key is required")
	}
	if timeout <= 0 || len(allowed) == 0 {
		return nil, errors.New("sub2api: timeout and at least one endpoint are required")
	}
	set := make(map[Endpoint]bool, len(allowed))
	for _, endpoint := range allowed {
		set[endpoint] = true
	}
	return &Client{base: strings.TrimRight(u.String(), "/") + "/api/v1/admin", key: apiKey,
		http:    &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		allowed: set}, nil
}

func (c *Client) call(ctx context.Context, endpoint Endpoint, id int64, query url.Values, body, out any) error {
	if c == nil || !c.allowed[endpoint] {
		return fmt.Errorf("sub2api: %s %s is not allowed for this caller", endpoint.Method, endpoint.Path)
	}
	path := endpoint.Path
	if strings.Contains(path, "{id}") {
		if id <= 0 {
			return errors.New("sub2api: account id must be positive")
		}
		path = strings.Replace(path, "{id}", strconv.FormatInt(id, 10), 1)
	}
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, endpoint.Method, target, reader)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", c.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("sub2api: %s %s: %w", endpoint.Method, endpoint.Path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("sub2api: read %s: %w", endpoint.Path, err)
	}
	if len(raw) > maxResponseBytes {
		return fmt.Errorf("sub2api: %s response exceeds %d bytes", endpoint.Path, maxResponseBytes)
	}
	var envelope struct {
		Code    *int            `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Code == nil {
		return fmt.Errorf("sub2api: %s returned HTTP %d without an API envelope", endpoint.Path, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK || *envelope.Code != 0 {
		return fmt.Errorf("sub2api: %s returned HTTP %d code %d: %.200s", endpoint.Path, resp.StatusCode, *envelope.Code, envelope.Message)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("sub2api: decode %s: %w", endpoint.Path, err)
	}
	return nil
}

// Overview is the request/error summary of a recent window (dashboard overview).
type Overview struct {
	RequestCountTotal            int64   `json:"request_count_total"`
	RequestCountSLA              int64   `json:"request_count_sla"`
	SuccessCount                 int64   `json:"success_count"`
	ErrorCountTotal              int64   `json:"error_count_total"`
	ErrorCountSLA                int64   `json:"error_count_sla"`
	BusinessLimitedCount         int64   `json:"business_limited_count"`
	UpstreamErrorCountExcl429529 int64   `json:"upstream_error_count_excl_429_529"`
	Upstream429Count             int64   `json:"upstream_429_count"`
	Upstream529Count             int64   `json:"upstream_529_count"`
	Duration                     Latency `json:"duration"`
}

type Latency struct {
	P95 *int `json:"p95_ms"`
	P99 *int `json:"p99_ms"`
}

// Overview reads the summary of the last window, one of sub2api's fixed
// ranges (5m, 30m, 1h).
func (c *Client) Overview(ctx context.Context, window string) (Overview, error) {
	var out Overview
	err := c.call(ctx, OpsOverview, 0, url.Values{"time_range": {window}}, nil, &out)
	return out, err
}

type GroupAvailability struct {
	GroupID        int64  `json:"group_id"`
	GroupName      string `json:"group_name"`
	TotalAccounts  int64  `json:"total_accounts"`
	AvailableCount int64  `json:"available_count"`
	RateLimitCount int64  `json:"rate_limit_count"`
	ErrorCount     int64  `json:"error_count"`
}

type AccountAvailability struct {
	AccountID              int64      `json:"account_id"`
	GroupID                int64      `json:"group_id"`
	Status                 string     `json:"status"`
	IsAvailable            bool       `json:"is_available"`
	IsRateLimited          bool       `json:"is_rate_limited"`
	IsOverloaded           bool       `json:"is_overloaded"`
	HasError               bool       `json:"has_error"`
	TempUnschedulableUntil *time.Time `json:"temp_unschedulable_until"`
}

// Availability is the realtime scheduling state of every account. Enabled is
// false when sub2api's realtime monitoring is switched off; the maps are then empty.
type Availability struct {
	Enabled  bool                           `json:"enabled"`
	Groups   map[string]GroupAvailability   `json:"group"`
	Accounts map[string]AccountAvailability `json:"account"`
}

func (c *Client) Availability(ctx context.Context) (Availability, error) {
	var out Availability
	err := c.call(ctx, OpsAvailability, 0, nil, nil, &out)
	return out, err
}

type UpstreamError struct {
	AccountID  *int64    `json:"account_id"`
	GroupID    *int64    `json:"group_id"`
	StatusCode int       `json:"status_code"`
	CreatedAt  time.Time `json:"created_at"`
}

// UpstreamErrors is one page of provider-side errors; Total counts all matches,
// so Total > len(Items) means the page is a sample, not a complete count.
type UpstreamErrors struct {
	Total int64           `json:"total"`
	Items []UpstreamError `json:"items"`
}

func (c *Client) UpstreamErrors(ctx context.Context, window string, pageSize int) (UpstreamErrors, error) {
	var out UpstreamErrors
	err := c.call(ctx, OpsUpstreamErrors, 0, url.Values{"time_range": {window}, "page_size": {strconv.Itoa(pageSize)}}, nil, &out)
	return out, err
}

// Account holds only the scheduling fields; credentials are never decoded.
type Account struct {
	ID                     int64      `json:"id"`
	Platform               string     `json:"platform"`
	Status                 string     `json:"status"`
	Schedulable            bool       `json:"schedulable"`
	GroupIDs               []int64    `json:"group_ids"`
	RateLimitResetAt       *time.Time `json:"rate_limit_reset_at"`
	OverloadUntil          *time.Time `json:"overload_until"`
	TempUnschedulableUntil *time.Time `json:"temp_unschedulable_until"`
}

func (c *Client) Account(ctx context.Context, id int64) (Account, error) {
	var out Account
	err := c.call(ctx, GetAccount, id, nil, nil, &out)
	if err == nil && out.ID != id {
		return Account{}, fmt.Errorf("sub2api: account %d response names account %d", id, out.ID)
	}
	return out, err
}

// SetSchedulable is not version-conditional in sub2api: callers must read the
// current state immediately before, and record that the race window exists.
func (c *Client) SetSchedulable(ctx context.Context, id int64, schedulable bool) (Account, error) {
	var out Account
	err := c.call(ctx, SetSchedulable, id, nil, map[string]bool{"schedulable": schedulable}, &out)
	if err == nil && (out.ID != id || out.Schedulable != schedulable) {
		return Account{}, fmt.Errorf("sub2api: account %d schedulable write was not reflected", id)
	}
	return out, err
}
