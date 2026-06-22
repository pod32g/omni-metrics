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

func TestPushConfigDefaults(t *testing.T) {
	// No push block => enabled with the default 16 MiB body cap.
	c, err := LoadBytes([]byte("web:\n  listen: 127.0.0.1:9090\n"))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if !c.Push.IsEnabled() {
		t.Error("push should default to enabled")
	}
	if c.Push.BodyLimit() != 16<<20 {
		t.Errorf("default body limit = %d, want %d", c.Push.BodyLimit(), 16<<20)
	}
	if c.Push.SampleLimit != 0 {
		t.Errorf("default sample limit = %d, want 0", c.Push.SampleLimit)
	}
}

func TestPushConfigExplicit(t *testing.T) {
	yaml := `
push:
  enabled: false
  sample_limit: 500
  max_body_bytes: 1048576
  auth_token: s3cr3t
`
	c, err := LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if c.Push.IsEnabled() {
		t.Error("push should be disabled")
	}
	if c.Push.BodyLimit() != 1<<20 {
		t.Errorf("body limit = %d", c.Push.BodyLimit())
	}
	if c.Push.SampleLimit != 500 || c.Push.AuthToken != "s3cr3t" {
		t.Errorf("push config = %+v", c.Push)
	}
}

func TestDefaultEnablesPush(t *testing.T) {
	if !Default().Push.IsEnabled() {
		t.Error("Default() push should be enabled")
	}
}

func TestLoadBytesParsesAuthAndTLS(t *testing.T) {
	yaml := `
scrape_configs:
  - job_name: id
    scheme: https
    authorization:
      type: Bearer
      credentials: tok123
    tls_config:
      ca_file: /etc/ca.pem
      server_name: id.internal
      insecure_skip_verify: true
    static_configs:
      - targets: [id:8081]
`
	c, err := LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	sc := c.ScrapeConfigs[0]
	if sc.Scheme != "https" {
		t.Errorf("scheme = %q, want https", sc.Scheme)
	}
	if sc.Authorization == nil || sc.Authorization.Credentials != "tok123" || sc.Authorization.Type != "Bearer" {
		t.Errorf("authorization = %+v", sc.Authorization)
	}
	if sc.TLS == nil || sc.TLS.CAFile != "/etc/ca.pem" || !sc.TLS.InsecureSkipVerify || sc.TLS.ServerName != "id.internal" {
		t.Errorf("tls_config = %+v", sc.TLS)
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("TOK", "secret")
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "plain", want: "plain"},
		{in: "${TOK}", want: "secret"},
		{in: "Bearer ${TOK}", want: "Bearer secret"},
		{in: "${MISSING:-def}", want: "def"},
		{in: "${TOK:-def}", want: "secret"},
		{in: "${MISSING}", wantErr: true},
	}
	for _, c := range cases {
		got, err := expandEnv(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("expandEnv(%q): expected error", c.in)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("expandEnv(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestValidateAuthRules(t *testing.T) {
	cases := map[string]string{
		"bad scheme": `
scrape_configs:
  - job_name: x
    scheme: ftp
    static_configs: [{targets: [h:1]}]`,
		"auth and basic_auth": `
scrape_configs:
  - job_name: x
    authorization: {credentials: a}
    basic_auth: {username: u, password: p}
    static_configs: [{targets: [h:1]}]`,
		"credentials and credentials_file": `
scrape_configs:
  - job_name: x
    authorization: {credentials: a, credentials_file: /f}
    static_configs: [{targets: [h:1]}]`,
		"cert without key": `
scrape_configs:
  - job_name: x
    tls_config: {cert_file: /c}
    static_configs: [{targets: [h:1]}]`,
	}
	for name, y := range cases {
		if _, err := LoadBytes([]byte(y)); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}
