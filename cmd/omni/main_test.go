package main

import (
	"github.com/pod32g/omni-metrics/internal/config"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDoHealthcheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()
	if err := doHealthcheck(ok.URL, time.Second); err != nil {
		t.Errorf("200 should pass: %v", err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	if err := doHealthcheck(bad.URL, time.Second); err == nil {
		t.Error("500 should fail the healthcheck")
	}

	if err := doHealthcheck("http://127.0.0.1:1/nope", 300*time.Millisecond); err == nil {
		t.Error("unreachable endpoint should fail the healthcheck")
	}
}

func TestHealthcheckCmdArgs(t *testing.T) {
	if code := healthcheckCmd([]string{}); code == 0 {
		t.Error("missing -url should be a non-zero exit")
	}
}

func TestSelfScrapeTarget(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0:9090":   "127.0.0.1:9090", // wildcard bind -> loopback scrape
		"127.0.0.1:9090": "127.0.0.1:9090",
		":9090":          "127.0.0.1:9090",
		"[::]:9090":      "127.0.0.1:9090",
		"host:1234":      "host:1234",
	}
	for in, want := range cases {
		if got := selfScrapeTarget(in); got != want {
			t.Errorf("selfScrapeTarget(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestToScrapeConfigsCarriesAuthAndTLS(t *testing.T) {
	c, err := config.LoadBytes([]byte(`
scrape_configs:
  - job_name: id
    scheme: https
    authorization: {type: Bearer, credentials: tok}
    tls_config: {insecure_skip_verify: true}
    static_configs: [{targets: [id:8081]}]
  - job_name: app
    basic_auth: {username: u, password: p}
    static_configs: [{targets: [app:9090]}]
`))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	got, err := toScrapeConfigs(c)
	if err != nil {
		t.Fatalf("toScrapeConfigs: %v", err)
	}
	if got[0].Scheme != "https" || got[0].Auth.Credentials != "tok" || got[0].TLS == nil || !got[0].TLS.InsecureSkipVerify {
		t.Errorf("job id = %+v", got[0])
	}
	if got[1].Auth.BasicUser != "u" || got[1].Auth.BasicPass != "p" || !got[1].Auth.HasBasic {
		t.Errorf("job app auth = %+v", got[1].Auth)
	}
}

func TestBuildAlertDatasourcesAddsBuiltinLocal(t *testing.T) {
	cfg := config.Default()
	cfg.Web.Listen = "0.0.0.0:9090"
	dss, def := buildAlertDatasources(cfg)
	if def != "local" {
		t.Errorf("default = %q, want local", def)
	}
	var local *struct{ url, source string }
	for _, d := range dss {
		if d.Name == "local" {
			local = &struct{ url, source string }{d.BaseURL, d.Source}
		}
	}
	if local == nil {
		t.Fatal("builtin local datasource missing")
	}
	if local.url != "http://127.0.0.1:9090" {
		t.Errorf("local base_url = %q, want loopback rewrite", local.url)
	}
	if local.source != "builtin" {
		t.Errorf("local source = %q, want builtin", local.source)
	}
}

func TestBuildAlertDatasourcesMapsConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Web.Listen = "127.0.0.1:9090"
	cfg.Alerting.Datasources = []config.AlertDatasourceConfig{{
		Name:          "remote",
		Type:          "prometheus",
		URL:           "https://prom.example",
		Timeout:       config.Duration(5 * time.Second),
		Authorization: &config.Authorization{Credentials: "tok"},
		Headers:       map[string]string{"X-Scope-OrgID": "t"},
	}}
	dss, _ := buildAlertDatasources(cfg)
	var remote bool
	for _, d := range dss {
		if d.Name == "remote" {
			remote = true
			if d.BaseURL != "https://prom.example" || d.TimeoutMS != 5000 {
				t.Errorf("remote mapping = %+v", d)
			}
			if string(d.AuthType) != "bearer" || d.Credentials != "tok" {
				t.Errorf("remote auth = %v/%q", d.AuthType, d.Credentials)
			}
			if d.Source != "config" || d.Headers["X-Scope-OrgID"] != "t" {
				t.Errorf("remote source/headers = %+v", d)
			}
		}
	}
	if !remote {
		t.Fatal("remote datasource missing")
	}
}
