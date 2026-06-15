package main

import (
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
