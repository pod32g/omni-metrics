package config

import (
	"testing"
	"time"
)

func TestLoadBytesParsesNotify(t *testing.T) {
	t.Setenv("NTOK", "env-secret")
	yaml := "" +
		"alerting:\n" +
		"  notify:\n" +
		"    enabled: true\n" +
		"    url: http://notify:8088\n" +
		"    token: ${NTOK}\n" +
		"    min_severity: warning\n"
	c, err := LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	n := c.Alerting.Notify
	if !n.IsEnabled() {
		t.Errorf("IsEnabled = false, want true")
	}
	if n.URL != "http://notify:8088" {
		t.Errorf("url = %q", n.URL)
	}
	if n.Token != "env-secret" {
		t.Errorf("token = %q, want env-expanded", n.Token)
	}
	if n.MinSeverity != "warning" {
		t.Errorf("min_severity = %q", n.MinSeverity)
	}
	if n.Source != "omni-metrics" {
		t.Errorf("source default = %q, want omni-metrics", n.Source)
	}
	if time.Duration(n.Timeout) != 5*time.Second {
		t.Errorf("timeout default = %v, want 5s", time.Duration(n.Timeout))
	}
	if n.QueueSize != 1024 {
		t.Errorf("queue_size default = %d, want 1024", n.QueueSize)
	}
	if n.MaxRetries == nil || *n.MaxRetries != 3 {
		t.Errorf("max_retries default = %v, want 3", n.MaxRetries)
	}
}

func TestNotifyDisabledByDefault(t *testing.T) {
	c, err := LoadBytes([]byte("web:\n  listen: 127.0.0.1:9090\n"))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if c.Alerting.Notify.IsEnabled() {
		t.Errorf("notify should be disabled by default")
	}
}

func TestNotifyValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"missing url", "alerting:\n  notify:\n    enabled: true\n    token: t\n"},
		{"missing token", "alerting:\n  notify:\n    enabled: true\n    url: http://x:8088\n"},
		{"bad min_severity", "alerting:\n  notify:\n    enabled: true\n    url: http://x:8088\n    token: t\n    min_severity: high\n"},
		{"bad url scheme", "alerting:\n  notify:\n    enabled: true\n    url: ftp://x\n    token: t\n"},
		{"url with userinfo", "alerting:\n  notify:\n    enabled: true\n    url: http://user:pass@x:8088\n    token: t\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := LoadBytes([]byte(c.yaml)); err == nil {
				t.Errorf("expected validation error")
			}
		})
	}
}

func TestNotifyDisabledIgnoresUnresolvableToken(t *testing.T) {
	// A disabled block must not fail load even if its token env is unset.
	yaml := "alerting:\n  notify:\n    enabled: false\n    token: ${DEFINITELY_UNSET_NOTIFY_TOK}\n"
	if _, err := LoadBytes([]byte(yaml)); err != nil {
		t.Errorf("disabled notify should not fail load: %v", err)
	}
}

func TestNotifyEnabledUnsetTokenEnvIsLoud(t *testing.T) {
	yaml := "alerting:\n  notify:\n    enabled: true\n    url: http://x:8088\n    token: ${DEFINITELY_UNSET_NOTIFY_TOK}\n"
	if _, err := LoadBytes([]byte(yaml)); err == nil {
		t.Errorf("enabled notify with unset token env should fail load")
	}
}

func TestNotifyMaxRetriesZeroPreserved(t *testing.T) {
	yaml := "alerting:\n  notify:\n    enabled: true\n    url: http://x:8088\n    token: t\n    max_retries: 0\n"
	c, err := LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if c.Alerting.Notify.MaxRetries == nil || *c.Alerting.Notify.MaxRetries != 0 {
		t.Errorf("explicit max_retries: 0 should be preserved, got %v", c.Alerting.Notify.MaxRetries)
	}
}
