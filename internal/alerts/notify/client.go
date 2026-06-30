package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrPermanent marks a send failure that must not be retried (a 4xx response:
// the request itself is bad — wrong token, malformed event). Retryable failures
// (5xx, transport errors) are returned without wrapping it.
var ErrPermanent = errors.New("omni-notify rejected event")

// eventsPath is omni-notify's ingestion endpoint.
const eventsPath = "/api/v1/events"

// Client posts a single Notification to omni-notify as one event.
type Client struct {
	url    string
	token  string
	source string
	http   *http.Client
}

// NewClient builds a Client from cfg (defaults applied).
func NewClient(cfg Config) *Client {
	cfg = cfg.withDefaults()
	return &Client{
		url:    strings.TrimRight(cfg.URL, "/") + eventsPath,
		token:  cfg.Token,
		source: cfg.Source,
		http:   &http.Client{Timeout: cfg.Timeout},
	}
}

// event is omni-notify's ingestion schema.
type event struct {
	EventID     string            `json:"event_id"`
	Fingerprint string            `json:"fingerprint"`
	Type        string            `json:"type"`
	Source      string            `json:"source"`
	Status      string            `json:"status"`
	Severity    string            `json:"severity"`
	Title       string            `json:"title"`
	Summary     string            `json:"summary,omitempty"`
	Description string            `json:"description,omitempty"`
	Timestamp   string            `json:"timestamp"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Send delivers n as one omni-notify event. A 2xx response is success; a 4xx
// response yields an error wrapping ErrPermanent; 5xx and transport errors are
// returned plain (retryable).
func (c *Client) Send(ctx context.Context, n Notification) error {
	body, err := json.Marshal(c.toEvent(n))
	if err != nil {
		return fmt.Errorf("%w: marshal event: %v", ErrPermanent, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: build request: %v", ErrPermanent, err)
	}
	// Set Content-Length explicitly: a chunked body without it has tripped strict
	// receivers before in this codebase.
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("post event: %w", err) // transport error: retryable
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
	}()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return fmt.Errorf("%w: status %d", ErrPermanent, resp.StatusCode)
	default:
		return fmt.Errorf("omni-notify status %d", resp.StatusCode) // 5xx: retryable
	}
}

// toEvent maps a Notification to the omni-notify event schema.
func (c *Client) toEvent(n Notification) event {
	id := n.RuleID + ":" + n.Fingerprint
	summary := n.Annotations["summary"]
	if summary == "" {
		summary = n.RuleName
	}
	return event{
		EventID:     id,
		Fingerprint: id,
		Type:        "alert",
		Source:      c.source,
		Status:      n.Status,
		Severity:    MapSeverity(n.Severity),
		Title:       n.RuleName,
		Summary:     summary,
		Description: n.Annotations["description"],
		Timestamp:   n.Time.UTC().Format(time.RFC3339),
		Labels:      n.Labels,
		Annotations: n.Annotations,
	}
}
