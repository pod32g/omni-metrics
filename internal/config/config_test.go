package config

import (
	"testing"
	"time"
)

func TestDefaultHasSelfScrape(t *testing.T) {
	c := Default()
	if c.Web.Listen == "" {
		t.Errorf("default listen empty")
	}
	if c.Global.ScrapeInterval.D() != 15*time.Second {
		t.Errorf("default interval = %v, want 15s", c.Global.ScrapeInterval.D())
	}
	if len(c.ScrapeConfigs) != 1 || c.ScrapeConfigs[0].JobName != "omni" {
		t.Fatalf("expected a self-scrape 'omni' job, got %+v", c.ScrapeConfigs)
	}
}

func TestLoadBytes(t *testing.T) {
	yaml := `
global:
  scrape_interval: 30s
  scrape_timeout: 5s
storage:
  path: /tmp/omni-data
  retention: 12h
web:
  listen: 127.0.0.1:8080
scrape_configs:
  - job_name: node
    scrape_interval: 1m
    static_configs:
      - targets: [node-01:9100, node-02:9100]
`
	c, err := LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if c.Global.ScrapeInterval.D() != 30*time.Second {
		t.Errorf("interval = %v", c.Global.ScrapeInterval.D())
	}
	if c.Storage.Retention.D() != 12*time.Hour {
		t.Errorf("retention = %v", c.Storage.Retention.D())
	}
	if c.Web.Listen != "127.0.0.1:8080" {
		t.Errorf("listen = %q", c.Web.Listen)
	}
	if len(c.ScrapeConfigs) != 1 || len(c.ScrapeConfigs[0].StaticConfigs[0].Targets) != 2 {
		t.Fatalf("scrape configs = %+v", c.ScrapeConfigs)
	}
	if c.ScrapeConfigs[0].ScrapeInterval.D() != time.Minute {
		t.Errorf("job interval = %v", c.ScrapeConfigs[0].ScrapeInterval.D())
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	// Missing global/web should be filled with defaults.
	c, err := LoadBytes([]byte("scrape_configs:\n  - job_name: x\n    static_configs:\n      - targets: [localhost:1234]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Global.ScrapeInterval.D() != 15*time.Second {
		t.Errorf("default interval not applied: %v", c.Global.ScrapeInterval.D())
	}
	if c.Web.Listen == "" {
		t.Errorf("default listen not applied")
	}
}

func TestDurationParsing(t *testing.T) {
	cases := map[string]time.Duration{
		"15s": 15 * time.Second,
		"5m":  5 * time.Minute,
		"1h":  time.Hour,
		"2d":  48 * time.Hour,
		"1w":  7 * 24 * time.Hour,
	}
	for in, want := range cases {
		got, err := parseDuration(in)
		if err != nil || got != want {
			t.Errorf("parseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseDuration("nonsense"); err == nil {
		t.Errorf("expected error for bad duration")
	}
}

func TestValidateErrors(t *testing.T) {
	bad := []string{
		"scrape_configs:\n  - job_name: \"\"\n    static_configs:\n      - targets: [x:1]\n",
		"scrape_configs:\n  - job_name: y\n    static_configs:\n      - targets: []\n",
	}
	for _, y := range bad {
		if _, err := LoadBytes([]byte(y)); err == nil {
			t.Errorf("expected validation error for:\n%s", y)
		}
	}
}
