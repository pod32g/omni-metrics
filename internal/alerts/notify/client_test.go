package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testNotification() Notification {
	return Notification{
		RuleID:      "rule-1",
		RuleName:    "High CPU",
		Fingerprint: "abc123",
		Status:      "firing",
		Severity:    "critical",
		Value:       0.97,
		Labels:      map[string]string{"instance": "node-1", "job": "node"},
		Annotations: map[string]string{"summary": "CPU is hot", "description": "over 95% for 5m"},
		Time:        time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC),
	}
}

func TestClientSendBuildsEvent(t *testing.T) {
	var (
		gotMethod, gotPath, gotAuth, gotCT string
		gotLen                             int64
		body                               map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotLen = r.ContentLength
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := NewClient(Config{URL: srv.URL, Token: "secret-tok", Source: "omni-metrics"})
	if err := c.Send(context.Background(), testNotification()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/v1/events" {
		t.Errorf("path = %q, want /api/v1/events", gotPath)
	}
	if gotAuth != "Bearer secret-tok" {
		t.Errorf("auth = %q, want Bearer secret-tok", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotCT)
	}
	if gotLen <= 0 {
		t.Errorf("Content-Length = %d, want > 0", gotLen)
	}

	want := map[string]string{
		"event_id":    "rule-1:abc123",
		"fingerprint": "rule-1:abc123",
		"type":        "alert",
		"source":      "omni-metrics",
		"status":      "firing",
		"severity":    "critical",
		"title":       "High CPU",
		"summary":     "CPU is hot",
		"description": "over 95% for 5m",
		"timestamp":   "2026-06-30T12:00:00Z",
	}
	for k, v := range want {
		if got, _ := body[k].(string); got != v {
			t.Errorf("event[%q] = %q, want %q", k, got, v)
		}
	}
	labels, _ := body["labels"].(map[string]any)
	if labels["instance"] != "node-1" || labels["job"] != "node" {
		t.Errorf("labels = %v, want instance=node-1 job=node", body["labels"])
	}
}

func TestClientSummaryFallsBackToTitle(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	n := testNotification()
	n.Annotations = nil // no summary/description
	c := NewClient(Config{URL: srv.URL, Token: "t"})
	if err := c.Send(context.Background(), n); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if body["summary"] != "High CPU" {
		t.Errorf("summary = %v, want title fallback High CPU", body["summary"])
	}
	if _, present := body["description"]; present {
		t.Errorf("description should be omitted when empty, got %v", body["description"])
	}
}

func TestClientSend4xxIsPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad token", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewClient(Config{URL: srv.URL, Token: "t"})
	err := c.Send(context.Background(), testNotification())
	if err == nil {
		t.Fatal("Send: want error on 401")
	}
	if !errors.Is(err, ErrPermanent) {
		t.Errorf("err = %v, want permanent", err)
	}
}

func TestClientSend5xxIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	c := NewClient(Config{URL: srv.URL, Token: "t"})
	err := c.Send(context.Background(), testNotification())
	if err == nil {
		t.Fatal("Send: want error on 502")
	}
	if errors.Is(err, ErrPermanent) {
		t.Errorf("err = %v, want retryable (not permanent)", err)
	}
}

func TestClientSendTransportErrorIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // server is down -> connection refused

	c := NewClient(Config{URL: srv.URL, Token: "t"})
	err := c.Send(context.Background(), testNotification())
	if err == nil {
		t.Fatal("Send: want transport error")
	}
	if errors.Is(err, ErrPermanent) {
		t.Errorf("err = %v, want retryable (not permanent)", err)
	}
}
