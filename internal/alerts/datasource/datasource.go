// Package datasource is the provider-agnostic query abstraction the alerting
// engine evaluates rules through. The only implementation today is a
// Prometheus-compatible HTTP client, but the Datasource interface keeps the rest
// of the engine from assuming the backend is the local omni instance.
package datasource

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pod32g/omni-metrics/internal/alerts/models"
)

// Datasource executes an instant PromQL query and returns a typed result.
type Datasource interface {
	Query(ctx context.Context, promql string, ts time.Time) (models.Result, error)
}

// prometheus is an HTTP client for the Prometheus-compatible instant query API.
type prometheus struct {
	cfg    models.Datasource
	client *http.Client
}

// New builds a Datasource from its configuration. The per-datasource timeout is
// applied to the HTTP client; a non-positive timeout falls back to 30s.
func New(cfg models.Datasource) Datasource {
	timeout := time.Duration(cfg.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &prometheus{cfg: cfg, client: &http.Client{Timeout: timeout}}
}

// promResponse is the Prometheus {status,data} envelope.
type promResponse struct {
	Status    string   `json:"status"`
	ErrorType string   `json:"errorType"`
	Error     string   `json:"error"`
	Data      promData `json:"data"`
}

type promData struct {
	ResultType string          `json:"resultType"`
	Result     json.RawMessage `json:"result"`
}

func (p *prometheus) Query(ctx context.Context, promql string, ts time.Time) (models.Result, error) {
	endpoint := p.cfg.BaseURL + "/api/v1/query"
	form := url.Values{}
	form.Set("query", promql)
	form.Set("time", strconv.FormatFloat(float64(ts.UnixMilli())/1000, 'f', -1, 64))

	// POST form so long queries don't hit URL length limits (Grafana does this
	// too). Passing the body to NewRequestWithContext sets ContentLength and
	// GetBody, so the body survives redirects.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return models.Result{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	p.applyAuth(req)

	resp, err := p.client.Do(req)
	if err != nil {
		return models.Result{}, fmt.Errorf("datasource request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return models.Result{}, fmt.Errorf("datasource HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var pr promResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return models.Result{}, fmt.Errorf("decoding datasource response: %w", err)
	}
	if pr.Status != "success" {
		msg := pr.Error
		if msg == "" {
			msg = "datasource reported error"
		}
		return models.Result{}, fmt.Errorf("datasource query error: %s", msg)
	}
	return decodeResult(pr.Data)
}

func (p *prometheus) applyAuth(req *http.Request) {
	for k, v := range p.cfg.Headers {
		req.Header.Set(k, v)
	}
	switch p.cfg.AuthType {
	case models.AuthBearer:
		if p.cfg.Credentials != "" {
			req.Header.Set("Authorization", "Bearer "+p.cfg.Credentials)
		}
	case models.AuthBasic:
		req.SetBasicAuth(p.cfg.BasicUser, p.cfg.BasicPass)
	}
}

// decodeResult maps a Prometheus result payload into a models.Result. A vector
// result becomes KindVector (or KindEmpty when there are no elements); a scalar
// becomes a single labelless KindScalar sample.
func decodeResult(d promData) (models.Result, error) {
	switch d.ResultType {
	case "vector":
		var raw []struct {
			Metric map[string]string  `json:"metric"`
			Value  [2]json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(d.Result, &raw); err != nil {
			return models.Result{}, fmt.Errorf("decoding vector: %w", err)
		}
		if len(raw) == 0 {
			return models.Result{Kind: models.KindEmpty}, nil
		}
		out := make([]models.Sample, 0, len(raw))
		for _, e := range raw {
			v, err := parseSampleValue(e.Value[1])
			if err != nil {
				return models.Result{}, err
			}
			out = append(out, models.Sample{Labels: e.Metric, Value: v})
		}
		return models.Result{Kind: models.KindVector, Samples: out}, nil
	case "scalar":
		var pair [2]json.RawMessage
		if err := json.Unmarshal(d.Result, &pair); err != nil {
			return models.Result{}, fmt.Errorf("decoding scalar: %w", err)
		}
		v, err := parseSampleValue(pair[1])
		if err != nil {
			return models.Result{}, err
		}
		return models.Result{Kind: models.KindScalar, Samples: []models.Sample{{Value: v}}}, nil
	case "matrix", "string":
		return models.Result{}, fmt.Errorf("unsupported result type %q for alerting (use an instant vector or scalar)", d.ResultType)
	default:
		return models.Result{}, fmt.Errorf("unknown result type %q", d.ResultType)
	}
}

// parseSampleValue parses a Prometheus sample value, which is JSON-encoded as a
// quoted string ("0", "NaN", "+Inf"). It also tolerates a bare JSON number.
func parseSampleValue(raw json.RawMessage) (float64, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		// Fall back to a bare number.
		var f float64
		if err2 := json.Unmarshal(raw, &f); err2 != nil {
			return 0, fmt.Errorf("decoding sample value %s: %w", raw, err)
		}
		return f, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing sample value %q: %w", s, err)
	}
	return f, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
