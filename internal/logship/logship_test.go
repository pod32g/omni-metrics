package logship

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestWriterShipsLines(t *testing.T) {
	var mu sync.Mutex
	var recs []map[string]any
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		keys = append(keys, r.Header.Get("X-Api-Key"))
		sc := bufio.NewScanner(r.Body)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(bytes.TrimSpace(sc.Bytes()), &m) == nil {
				recs = append(recs, m)
			}
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	w, err := NewWriter(Config{URL: srv.URL, APIKey: "k", Service: "omni-metrics", FlushInterval: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate stdlib log writes (each ends with a newline).
	_, _ = w.Write([]byte("omni-metrics listening on 0.0.0.0:9090\n"))
	_, _ = w.Write([]byte("WARN: storage recovered with issues\n"))
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d: %+v", len(recs), recs)
	}
	for _, k := range keys {
		if k != "k" {
			t.Fatalf("bad api key %q", k)
		}
	}
	if recs[0]["service"] != "omni-metrics" || recs[0]["level"] != "info" {
		t.Fatalf("bad rec0: %+v", recs[0])
	}
	if recs[1]["level"] != "warn" {
		t.Fatalf("want warn level for WARN line, got %+v", recs[1])
	}
}
