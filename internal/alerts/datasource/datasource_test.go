package datasource_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pod32g/omni-metrics/internal/alerts/datasource"
	"github.com/pod32g/omni-metrics/internal/alerts/models"
)

func newDS(t *testing.T, baseURL string, mut func(*models.Datasource)) datasource.Datasource {
	t.Helper()
	d := models.Datasource{BaseURL: baseURL, Type: "prometheus", TimeoutMS: 2000, Enabled: true}
	if mut != nil {
		mut(&d)
	}
	return datasource.New(d)
}

func TestQueryVector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if q := r.FormValue("query"); q != "up == 0" {
			t.Errorf("query = %q", q)
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` +
			`{"metric":{"instance":"a","job":"x"},"value":[1.5,"0"]},` +
			`{"metric":{"instance":"b"},"value":[1.5,"3"]}]}}`))
	}))
	defer srv.Close()

	res, err := newDS(t, srv.URL, nil).Query(context.Background(), "up == 0", time.Unix(1, 0))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Kind != models.KindVector {
		t.Fatalf("kind = %v, want vector", res.Kind)
	}
	if len(res.Samples) != 2 {
		t.Fatalf("samples = %d, want 2", len(res.Samples))
	}
	if res.Samples[0].Labels["instance"] != "a" || res.Samples[0].Value != 0 {
		t.Errorf("sample0 = %+v", res.Samples[0])
	}
	if res.Samples[1].Value != 3 {
		t.Errorf("sample1 value = %v, want 3", res.Samples[1].Value)
	}
}

func TestQueryScalar(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"resultType":"scalar","result":[1.5,"42"]}}`))
	}))
	defer srv.Close()
	res, err := newDS(t, srv.URL, nil).Query(context.Background(), "vector(42)", time.Unix(1, 0))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Kind != models.KindScalar || len(res.Samples) != 1 || res.Samples[0].Value != 42 {
		t.Fatalf("scalar = %+v", res)
	}
	if len(res.Samples[0].Labels) != 0 {
		t.Errorf("scalar should have no labels, got %v", res.Samples[0].Labels)
	}
}

func TestQueryEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer srv.Close()
	res, err := newDS(t, srv.URL, nil).Query(context.Background(), "up == 0", time.Unix(1, 0))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Kind != models.KindEmpty || len(res.Samples) != 0 {
		t.Fatalf("empty = %+v", res)
	}
}

func TestQueryErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"parse error: bad query"}`))
	}))
	defer srv.Close()
	_, err := newDS(t, srv.URL, nil).Query(context.Background(), "garbage(", time.Unix(1, 0))
	if err == nil || !strings.Contains(err.Error(), "parse error") {
		t.Fatalf("err = %v, want parse error", err)
	}
}

func TestQueryHTTP401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("unauthorized"))
	}))
	defer srv.Close()
	_, err := newDS(t, srv.URL, nil).Query(context.Background(), "up", time.Unix(1, 0))
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want 401", err)
	}
}

func TestQueryBearerAuthAndHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok123" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-Scope-OrgID"); got != "tenant-a" {
			t.Errorf("X-Scope-OrgID = %q", got)
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer srv.Close()
	ds := newDS(t, srv.URL, func(d *models.Datasource) {
		d.AuthType = models.AuthBearer
		d.Credentials = "tok123"
		d.Headers = map[string]string{"X-Scope-OrgID": "tenant-a"}
	})
	if _, err := ds.Query(context.Background(), "up", time.Unix(1, 0)); err != nil {
		t.Fatalf("Query: %v", err)
	}
}

func TestQueryBasicAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "user" || p != "pw" {
			t.Errorf("basic auth = %q/%q ok=%v", u, p, ok)
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer srv.Close()
	ds := newDS(t, srv.URL, func(d *models.Datasource) {
		d.AuthType = models.AuthBasic
		d.BasicUser = "user"
		d.BasicPass = "pw"
	})
	if _, err := ds.Query(context.Background(), "up", time.Unix(1, 0)); err != nil {
		t.Fatalf("Query: %v", err)
	}
}

func TestQueryTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer srv.Close()
	ds := newDS(t, srv.URL, func(d *models.Datasource) { d.TimeoutMS = 30 })
	_, err := ds.Query(context.Background(), "up", time.Unix(1, 0))
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestQueryMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{not json`))
	}))
	defer srv.Close()
	if _, err := newDS(t, srv.URL, nil).Query(context.Background(), "up", time.Unix(1, 0)); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestQuerySpecialFloats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"NaN"]}]}}`))
	}))
	defer srv.Close()
	res, err := newDS(t, srv.URL, nil).Query(context.Background(), "x", time.Unix(1, 0))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Kind != models.KindVector || len(res.Samples) != 1 {
		t.Fatalf("res = %+v", res)
	}
	// NaN parses without error (value is NaN); just assert no crash and a sample.
}
