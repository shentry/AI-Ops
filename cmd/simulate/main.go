package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"time"
)

type webhookEnvelope struct {
	Version string          `json:"version"`
	Status  string          `json:"status"`
	Alerts  []simulateAlert `json:"alerts"`
}

type simulateAlert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     string            `json:"startsAt"`
	EndsAt       string            `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
}

func main() {
	url := flag.String("url", "http://127.0.0.1:8080/webhook/alertmanager", "webhook URL")
	token := flag.String("token", os.Getenv("AUTH_TOKEN"), "Bearer token")
	n := flag.Int("n", 1, "number of alerts")
	dup := flag.Float64("dup", 0, "duplicate ratio")
	resolved := flag.Bool("resolved", false, "send resolved alerts")
	flag.Parse()
	if err := simulate(*url, *token, *n, *dup, *resolved); err != nil {
		fmt.Fprintln(os.Stderr, "simulate:", err)
		os.Exit(1)
	}
}

func simulate(url, token string, n int, dup float64, resolved bool) error {
	if token == "" {
		return fmt.Errorf("token is required")
	}
	if n <= 0 {
		return fmt.Errorf("n must be greater than zero")
	}
	if dup < 0 || dup > 1 || math.IsNaN(dup) {
		return fmt.Errorf("dup must be between 0 and 1")
	}
	payload, err := buildPayload(n, dup, resolved, time.Now().UTC())
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("webhook returned HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func buildPayload(n int, dup float64, resolved bool, now time.Time) ([]byte, error) {
	unique := n - int(math.Round(float64(n)*dup))
	if unique < 1 {
		unique = 1
	}
	status, endsAt := "firing", "0001-01-01T00:00:00Z"
	if resolved {
		status, endsAt = "resolved", now.Format(time.RFC3339Nano)
	}
	alerts := make([]simulateAlert, n)
	for i := range alerts {
		identity := i
		if i >= unique {
			identity = i % unique
		}
		alerts[i] = simulateAlert{Status: status, Labels: map[string]string{"alertname": "SimulatedAlert", "instance": "node-" + strconv.Itoa(identity), "service": "payments", "severity": "critical"}, Annotations: map[string]string{"summary": "D03 simulated alert"}, StartsAt: now.Add(-time.Minute).Format(time.RFC3339Nano), EndsAt: endsAt, GeneratorURL: "http://127.0.0.1:9090/graph?g0.expr=vector(1)"}
	}
	return json.Marshal(webhookEnvelope{Version: "4", Status: status, Alerts: alerts})
}
