# Secure Metric Scraping Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give scrape jobs a Prometheus-shaped authentication (bearer + basic) and TLS surface, with secrets supplied via inline values, `_file` references, or `${ENV}` expansion — then wire the live omni-identity scrape end-to-end.

**Architecture:** Config-layer parsing/validation/secret-resolution in `internal/config`; transport-layer auth header + per-job TLS client in `internal/scrape`; `cmd/omni` translates resolved config into scrape jobs. The scrape package stays independent of the config package (the existing `toScrapeConfigs` translation boundary is preserved). TLS file *contents* are read when the per-job HTTP client is built; credential values are resolved (env-expanded, `_file`-read) once at config load.

**Tech Stack:** Go stdlib (`crypto/tls`, `net/http`, `gopkg.in/yaml.v3`), table-driven tests with `net/http/httptest`.

Spec: `docs/superpowers/specs/2026-06-21-secure-scraping-design.md`

---

## File Structure

- `internal/config/config.go` — add `Scheme`, `Authorization`, `BasicAuth`, `TLSConfig` to `ScrapeConfig`; env expansion; `_file` resolution; validation; `TLSConfig.Build()`.
- `internal/config/config_test.go` — table-driven tests for parsing, expansion, resolution, validation, TLS build.
- `internal/scrape/scrape.go` — `Auth` type + `apply`; per-job `*http.Client` from a `*tls.Config`; `Scheme` on `ScrapeConfig`; thread client+auth to each target.
- `internal/scrape/scrape_test.go` — bearer/basic header assertions; TLS server (CA-verified), mTLS, `insecure_skip_verify`; 401→`up=0`.
- `cmd/omni/main.go` — `toScrapeConfigs` maps resolved config → `scrape.ScrapeConfig`, calling `TLSConfig.Build()`.
- `cmd/omni/main_test.go` — `toScrapeConfigs` carries scheme/auth/TLS.
- `omni.yml`, `docker-compose.yml`, `.github/workflows/cicd.yml` — live omni-identity wiring.
- `README.md`, `examples/omni.yml` — documentation.

---

## Task 1: Config schema structs + parsing

**Files:**
- Modify: `internal/config/config.go` (the `ScrapeConfig` struct, ~line 70)
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestLoadBytesParsesAuthAndTLS -v`
Expected: FAIL — `sc.Scheme`, `sc.Authorization`, `sc.TLS` undefined.

- [ ] **Step 3: Write minimal implementation**

In `internal/config/config.go`, extend `ScrapeConfig` and add the new structs:

```go
// ScrapeConfig is one scrape job.
type ScrapeConfig struct {
	JobName        string         `yaml:"job_name"`
	Scheme         string         `yaml:"scheme"`
	ScrapeInterval Duration       `yaml:"scrape_interval"`
	ScrapeTimeout  Duration       `yaml:"scrape_timeout"`
	StaticConfigs  []StaticConfig `yaml:"static_configs"`
	Authorization  *Authorization `yaml:"authorization"`
	BasicAuth      *BasicAuth     `yaml:"basic_auth"`
	TLS            *TLSConfig     `yaml:"tls_config"`
}

// Authorization is the Prometheus bearer-style auth block. The rendered header
// is "<Type> <credentials>" (Type defaults to Bearer).
type Authorization struct {
	Type            string `yaml:"type"`
	Credentials     string `yaml:"credentials"`
	CredentialsFile string `yaml:"credentials_file"`
}

// BasicAuth carries HTTP basic-auth credentials.
type BasicAuth struct {
	Username     string `yaml:"username"`
	UsernameFile string `yaml:"username_file"`
	Password     string `yaml:"password"`
	PasswordFile string `yaml:"password_file"`
}

// TLSConfig configures the scrape transport's TLS. File fields are paths; their
// contents are read when the HTTP client is built (see Build).
type TLSConfig struct {
	CAFile             string `yaml:"ca_file"`
	CertFile           string `yaml:"cert_file"`
	KeyFile            string `yaml:"key_file"`
	ServerName         string `yaml:"server_name"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -run TestLoadBytesParsesAuthAndTLS -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
gofmt -w ./internal/config/
git add internal/config/config.go internal/config/config_test.go
git -c user.name="pod32g" -c user.email="3311662+pod32g@users.noreply.github.com" commit -m "feat(config): parse scrape auth/TLS blocks"
```

---

## Task 2: `${ENV}` expansion helper

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestExpandEnv -v`
Expected: FAIL — `expandEnv` undefined.

- [ ] **Step 3: Write minimal implementation**

Add to `internal/config/config.go` (import `regexp`, `strings`, `os` already present):

```go
// envRef matches ${VAR} or ${VAR:-default}.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// expandEnv replaces ${VAR} / ${VAR:-default} references. A reference to an unset
// variable with no default is an error — fail loud rather than scrape with an
// empty credential.
func expandEnv(s string) (string, error) {
	var bad string
	out := envRef.ReplaceAllStringFunc(s, func(m string) string {
		g := envRef.FindStringSubmatch(m)
		name, hasDef, def := g[1], g[2] != "", g[3]
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		if hasDef {
			return def
		}
		bad = name
		return ""
	})
	if bad != "" {
		return "", fmt.Errorf("environment variable %q is not set", bad)
	}
	return out, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -run TestExpandEnv -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
gofmt -w ./internal/config/
git add internal/config/config.go internal/config/config_test.go
git -c user.name="pod32g" -c user.email="3311662+pod32g@users.noreply.github.com" commit -m "feat(config): ${ENV} expansion helper with fail-loud on unset"
```

---

## Task 3: Validation rules

**Files:**
- Modify: `internal/config/config.go` (`validate`, ~line 148)
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestValidateAuthRules -v`
Expected: FAIL — these configs currently load without error.

- [ ] **Step 3: Write minimal implementation**

In `internal/config/config.go`, inside `validate()`'s per-`sc` loop (after the existing target check), add:

```go
		if sc.Scheme != "" && sc.Scheme != "http" && sc.Scheme != "https" {
			return fmt.Errorf("scrape config %q: scheme must be http or https", sc.JobName)
		}
		if sc.Authorization != nil && sc.BasicAuth != nil {
			return fmt.Errorf("scrape config %q: authorization and basic_auth are mutually exclusive", sc.JobName)
		}
		if a := sc.Authorization; a != nil && a.Credentials != "" && a.CredentialsFile != "" {
			return fmt.Errorf("scrape config %q: authorization credentials and credentials_file are mutually exclusive", sc.JobName)
		}
		if b := sc.BasicAuth; b != nil {
			if b.Password != "" && b.PasswordFile != "" {
				return fmt.Errorf("scrape config %q: basic_auth password and password_file are mutually exclusive", sc.JobName)
			}
			if b.Username != "" && b.UsernameFile != "" {
				return fmt.Errorf("scrape config %q: basic_auth username and username_file are mutually exclusive", sc.JobName)
			}
		}
		if tc := sc.TLS; tc != nil && (tc.CertFile == "") != (tc.KeyFile == "") {
			return fmt.Errorf("scrape config %q: tls_config cert_file and key_file must be set together", sc.JobName)
		}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -run TestValidateAuthRules -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
gofmt -w ./internal/config/
git add internal/config/config.go internal/config/config_test.go
git -c user.name="pod32g" -c user.email="3311662+pod32g@users.noreply.github.com" commit -m "feat(config): validate scrape auth/TLS mutual-exclusion + scheme"
```

---

## Task 4: Secret resolution (env expand + `_file` read)

**Files:**
- Modify: `internal/config/config.go` (`LoadBytes`, ~line 106)
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestResolveSecrets(t *testing.T) {
	t.Setenv("TOK", "envtok")
	dir := t.TempDir()
	pwFile := dir + "/pw"
	if err := os.WriteFile(pwFile, []byte("filepw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	yaml := `
scrape_configs:
  - job_name: id
    authorization: {credentials: "Bearer-${TOK}"}
    static_configs: [{targets: [h:1]}]
  - job_name: app
    basic_auth: {username: u, password_file: ` + pwFile + `}
    static_configs: [{targets: [h:2]}]
`
	c, err := LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if got := c.ScrapeConfigs[0].Authorization.Credentials; got != "Bearer-envtok" {
		t.Errorf("credentials = %q, want Bearer-envtok", got)
	}
	if got := c.ScrapeConfigs[1].BasicAuth.Password; got != "filepw" {
		t.Errorf("password from file = %q, want filepw (trimmed)", got)
	}
}

func TestResolveSecretsUnsetEnvErrors(t *testing.T) {
	yaml := `
scrape_configs:
  - job_name: id
    authorization: {credentials: "${DEFINITELY_UNSET_TOKEN}"}
    static_configs: [{targets: [h:1]}]
`
	if _, err := LoadBytes([]byte(yaml)); err == nil {
		t.Fatal("expected error for unset env var in credentials")
	}
}
```

(Ensure `os` is imported in the test file.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestResolveSecrets -v`
Expected: FAIL — credentials still literal `Bearer-${TOK}`, password empty.

- [ ] **Step 3: Write minimal implementation**

In `LoadBytes`, call `resolveSecrets` after `validate`:

```go
func LoadBytes(b []byte) (*Config, error) {
	c := &Config{}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	if err := c.resolveSecrets(); err != nil {
		return nil, err
	}
	return c, nil
}
```

Add the resolver. `readFileField` env-expands a path then reads+trims it:

```go
// resolveSecrets env-expands inline credential values and reads any *_file
// credential references into their inline fields. TLS file paths are expanded
// here too but read later, when the scrape client is built.
func (c *Config) resolveSecrets() error {
	for i := range c.ScrapeConfigs {
		sc := &c.ScrapeConfigs[i]
		if a := sc.Authorization; a != nil {
			v, err := resolveSecret("authorization.credentials", a.Credentials, a.CredentialsFile)
			if err != nil {
				return fmt.Errorf("scrape config %q: %w", sc.JobName, err)
			}
			a.Credentials = v
		}
		if b := sc.BasicAuth; b != nil {
			u, err := resolveSecret("basic_auth.username", b.Username, b.UsernameFile)
			if err != nil {
				return fmt.Errorf("scrape config %q: %w", sc.JobName, err)
			}
			p, err := resolveSecret("basic_auth.password", b.Password, b.PasswordFile)
			if err != nil {
				return fmt.Errorf("scrape config %q: %w", sc.JobName, err)
			}
			b.Username, b.Password = u, p
		}
		if tc := sc.TLS; tc != nil {
			for _, p := range []*string{&tc.CAFile, &tc.CertFile, &tc.KeyFile, &tc.ServerName} {
				v, err := expandEnv(*p)
				if err != nil {
					return fmt.Errorf("scrape config %q: %w", sc.JobName, err)
				}
				*p = v
			}
		}
	}
	return nil
}

// resolveSecret returns the env-expanded inline value, or the trimmed contents of
// the (env-expanded) file path when inline is empty.
func resolveSecret(field, inline, file string) (string, error) {
	if inline != "" {
		v, err := expandEnv(inline)
		if err != nil {
			return "", fmt.Errorf("%s: %w", field, err)
		}
		return v, nil
	}
	if file == "" {
		return "", nil
	}
	path, err := expandEnv(file)
	if err != nil {
		return "", fmt.Errorf("%s_file: %w", field, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s_file: %w", field, err)
	}
	return strings.TrimSpace(string(b)), nil
}
```

(Add `"strings"` to imports if not present.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -run TestResolveSecrets -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
gofmt -w ./internal/config/
git add internal/config/config.go internal/config/config_test.go
git -c user.name="pod32g" -c user.email="3311662+pod32g@users.noreply.github.com" commit -m "feat(config): resolve scrape credentials from env and _file"
```

---

## Task 5: `TLSConfig.Build()`

**Files:**
- Modify: `internal/config/config.go` (import `crypto/tls`, `crypto/x509`)
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestTLSConfigBuild(t *testing.T) {
	if tc := (*TLSConfig)(nil); tc.Build != nil {
	} // compile guard removed below

	// nil receiver → nil config, no error.
	var none *TLSConfig
	if cfg, err := none.Build(); err != nil || cfg != nil {
		t.Fatalf("nil.Build() = %v, %v; want nil,nil", cfg, err)
	}

	tc := &TLSConfig{ServerName: "id.internal", InsecureSkipVerify: true}
	cfg, err := tc.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if cfg == nil || cfg.ServerName != "id.internal" || !cfg.InsecureSkipVerify {
		t.Fatalf("cfg = %+v", cfg)
	}

	// A bad CA path is an error.
	bad := &TLSConfig{CAFile: "/no/such/ca.pem"}
	if _, err := bad.Build(); err == nil {
		t.Errorf("expected error for missing ca_file")
	}
}
```

Delete the stray compile-guard line; final test should start at the nil-receiver check.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestTLSConfigBuild -v`
Expected: FAIL — `Build` undefined.

- [ ] **Step 3: Write minimal implementation**

Add to `internal/config/config.go`:

```go
// Build constructs a *tls.Config from the file paths and options. A nil receiver
// returns (nil, nil) — no custom TLS. CA/cert/key files are read here.
func (t *TLSConfig) Build() (*tls.Config, error) {
	if t == nil {
		return nil, nil
	}
	cfg := &tls.Config{
		ServerName:         t.ServerName,
		InsecureSkipVerify: t.InsecureSkipVerify,
	}
	if t.CAFile != "" {
		pem, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, fmt.Errorf("tls ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tls ca_file %q: no certificates found", t.CAFile)
		}
		cfg.RootCAs = pool
	}
	if t.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("tls cert/key: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}
```

Add `"crypto/tls"` and `"crypto/x509"` to the import block.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -run TestTLSConfigBuild -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
gofmt -w ./internal/config/
git add internal/config/config.go internal/config/config_test.go
git -c user.name="pod32g" -c user.email="3311662+pod32g@users.noreply.github.com" commit -m "feat(config): build *tls.Config from tls_config block"
```

---

## Task 6: Scrape auth header + per-job client + scheme

**Files:**
- Modify: `internal/scrape/scrape.go` (`ScrapeConfig`, `Target`, `Run`, `loop`, `scrapeOnce`, `fetchAndParse`, `normalizeTarget`; import `crypto/tls`)
- Test: `internal/scrape/scrape_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestScrapeSendsBearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte("x 1\n"))
	}))
	defer srv.Close()

	db := newDB(t)
	m := NewManager(db, 0)
	tgt, _ := normalizeTarget("test", srv.URL+"/metrics", "http")
	tgt.auth = Auth{Type: "Bearer", Credentials: "tok123"}
	m.scrapeOnce(context.Background(), tgt, time.Second)

	if gotAuth != "Bearer tok123" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer tok123")
	}
}

func TestScrapeSendsBasicAuth(t *testing.T) {
	var u, p string
	var ok bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok = r.BasicAuth()
		w.Write([]byte("x 1\n"))
	}))
	defer srv.Close()

	db := newDB(t)
	m := NewManager(db, 0)
	tgt, _ := normalizeTarget("test", srv.URL+"/metrics", "http")
	tgt.auth = Auth{BasicUser: "user", BasicPass: "pass", HasBasic: true}
	m.scrapeOnce(context.Background(), tgt, time.Second)

	if !ok || u != "user" || p != "pass" {
		t.Errorf("basic auth = %q/%q (ok=%v), want user/pass", u, p, ok)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/scrape/ -run 'TestScrapeSends' -v`
Expected: FAIL — `normalizeTarget` takes 2 args; `Auth`/`tgt.auth` undefined.

- [ ] **Step 3: Write minimal implementation**

In `internal/scrape/scrape.go`:

Add the `Auth` type and `apply`:

```go
// Auth is the resolved per-job request authentication. Credentials (with Type,
// default Bearer) sets an Authorization header; HasBasic sends HTTP basic auth.
type Auth struct {
	Type        string
	Credentials string
	BasicUser   string
	BasicPass   string
	HasBasic    bool
}

func (a Auth) apply(req *http.Request) {
	switch {
	case a.Credentials != "":
		typ := a.Type
		if typ == "" {
			typ = "Bearer"
		}
		req.Header.Set("Authorization", typ+" "+a.Credentials)
	case a.HasBasic:
		req.SetBasicAuth(a.BasicUser, a.BasicPass)
	}
}
```

Extend `ScrapeConfig` and `Target`:

```go
type ScrapeConfig struct {
	JobName  string
	Scheme   string
	Interval time.Duration
	Timeout  time.Duration
	Targets  []string
	Auth     Auth
	TLS      *tls.Config
}

type Target struct {
	Job      string
	Instance string
	URL      string
	auth     Auth
	client   *http.Client
}
```

In `Run`, build a per-job client when the job has a TLS config and pass scheme/auth/client to each target:

```go
	for _, cfg := range configs {
		interval := cfg.Interval
		if interval <= 0 {
			interval = 15 * time.Second
		}
		timeout := cfg.Timeout
		if timeout <= 0 || timeout > interval {
			timeout = interval
		}
		client := m.client
		if cfg.TLS != nil {
			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.TLSClientConfig = cfg.TLS
			client = &http.Client{Transport: tr}
		}
		scheme := cfg.Scheme
		if scheme == "" {
			scheme = "http"
		}
		for _, raw := range cfg.Targets {
			tgt, err := normalizeTarget(cfg.JobName, raw, scheme)
			if err != nil {
				m.recordError(cfg.JobName, raw, fmt.Sprintf("invalid target: %v", err))
				continue
			}
			tgt.auth = cfg.Auth
			tgt.client = client
			wg.Add(1)
			go func(tgt Target) {
				defer wg.Done()
				m.loop(ctx, tgt, interval, timeout)
			}(tgt)
		}
	}
```

Change `normalizeTarget` to accept a default scheme:

```go
func normalizeTarget(job, raw, scheme string) (Target, error) {
	if scheme == "" {
		scheme = "http"
	}
	s := raw
	if !strings.Contains(s, "://") {
		s = scheme + "://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return Target{}, err
	}
	if u.Host == "" {
		return Target{}, fmt.Errorf("missing host in %q", raw)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/metrics"
	}
	return Target{Job: job, Instance: u.Host, URL: u.String()}, nil
}
```

In `fetchAndParse`, use the target's client (fall back to the manager's) and apply auth:

```go
func (m *Manager) fetchAndParse(ctx context.Context, tgt Target, timeout time.Duration) (series []exposition.Series, fatal error, parseWarn error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, tgt.URL, nil)
	if err != nil {
		return nil, err, nil
	}
	req.Header.Set("Accept", "text/plain;version=0.0.4")
	tgt.auth.apply(req)
	client := tgt.client
	if client == nil {
		client = m.client
	}
	resp, err := client.Do(req)
	...
```

(Add `"crypto/tls"` to the scrape import block. Leave the rest of `fetchAndParse` unchanged.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/scrape/ -run 'TestScrapeSends' -v`
Expected: PASS. Then `go test ./internal/scrape/ -v` — the existing tests still call `normalizeTarget(job, url)` and must be updated to pass `"http"` as the third arg (update the four call sites in `scrape_test.go`).

- [ ] **Step 5: Commit**

```bash
gofmt -w ./internal/scrape/
go test ./internal/scrape/
git add internal/scrape/scrape.go internal/scrape/scrape_test.go
git -c user.name="pod32g" -c user.email="3311662+pod32g@users.noreply.github.com" commit -m "feat(scrape): per-job auth header, scheme, and TLS client"
```

---

## Task 7: Scrape TLS integration (CA-verified, mTLS, skip-verify)

**Files:**
- Test: `internal/scrape/scrape_test.go` (import `crypto/tls`, `crypto/x509`)

- [ ] **Step 1: Write the failing test**

```go
func TestScrapeTLSVerifiesWithCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("x 1\n"))
	}))
	defer srv.Close()

	// Trust the test server's self-signed cert via a CA pool.
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	db := newDB(t)
	m := NewManager(db, 0)
	cfg := ScrapeConfig{
		JobName: "tls",
		Targets: []string{srv.URL},
		TLS:     &tls.Config{RootCAs: pool},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx, []ScrapeConfig{cfg})
	// give the immediate scrape a moment, then stop.
	waitForUp(t, db)
	cancel()

	if v, ok := latest(t, db, "up", ""); !ok || v != 1 {
		t.Errorf("up = %v %v, want 1 over verified TLS", v, ok)
	}
}

func TestScrapeTLSInsecureSkipVerify(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("x 1\n"))
	}))
	defer srv.Close()

	db := newDB(t)
	m := NewManager(db, 0)
	cfg := ScrapeConfig{
		JobName: "tls",
		Targets: []string{srv.URL},
		TLS:     &tls.Config{InsecureSkipVerify: true},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx, []ScrapeConfig{cfg})
	waitForUp(t, db)
	cancel()

	if v, ok := latest(t, db, "up", ""); !ok || v != 1 {
		t.Errorf("up = %v %v, want 1 with insecure_skip_verify", v, ok)
	}
}
```

Add a small polling helper near the top of the test file:

```go
// waitForUp polls until an up sample exists or a short deadline passes.
func waitForUp(t *testing.T, db *tsdb.DB) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := latest(t, db, "up", ""); ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/scrape/ -run TestScrapeTLS -v`
Expected: initially FAIL if the scheme/URL handling doesn't carry the `https://` from `srv.URL` (httptest TLS URLs are already `https://…`, so `normalizeTarget` must keep an explicit scheme — confirm it does). If Task 6 is correct these should pass; if they fail, the bug is real and must be fixed in `normalizeTarget`/`Run`.

- [ ] **Step 3: Implementation**

No new production code expected — these tests exercise Task 6's TLS client path. If `TestScrapeTLSVerifiesWithCA` fails with a certificate error while skip-verify passes, the per-job client wiring in `Run` is wrong; fix it there.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/scrape/ -run TestScrapeTLS -v -race`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
gofmt -w ./internal/scrape/
git add internal/scrape/scrape_test.go
git -c user.name="pod32g" -c user.email="3311662+pod32g@users.noreply.github.com" commit -m "test(scrape): TLS verification, skip-verify integration"
```

---

## Task 8: Wire resolved config into scrape jobs

**Files:**
- Modify: `cmd/omni/main.go` (`toScrapeConfigs`, ~line 183)
- Test: `cmd/omni/main_test.go`

- [ ] **Step 1: Write the failing test**

```go
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
```

(Ensure `cmd/omni/main_test.go` imports `"github.com/pod32g/omni-metrics/internal/config"`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/omni/ -run TestToScrapeConfigsCarriesAuthAndTLS -v`
Expected: FAIL — `toScrapeConfigs` returns one value (no error) and ignores auth/TLS.

- [ ] **Step 3: Write minimal implementation**

Change `toScrapeConfigs` to return an error and map the new fields. In `main.go`, update its single caller (`go mgr.Run(ctx, toScrapeConfigs(cfg))`) to handle the error (log.Fatalf on failure, since a bad TLS file at startup should not start a half-configured scraper):

```go
func toScrapeConfigs(cfg *config.Config) ([]scrape.ScrapeConfig, error) {
	out := make([]scrape.ScrapeConfig, 0, len(cfg.ScrapeConfigs))
	for _, sc := range cfg.ScrapeConfigs {
		tlsCfg, err := sc.TLS.Build()
		if err != nil {
			return nil, fmt.Errorf("scrape config %q: %w", sc.JobName, err)
		}
		out = append(out, scrape.ScrapeConfig{
			JobName:  sc.JobName,
			Scheme:   sc.Scheme,
			Interval: sc.ScrapeInterval.D(),
			Timeout:  sc.ScrapeTimeout.D(),
			Targets:  sc.AllTargets(),
			Auth:     toAuth(sc),
			TLS:      tlsCfg,
		})
	}
	return out, nil
}

func toAuth(sc config.ScrapeConfig) scrape.Auth {
	if a := sc.Authorization; a != nil && a.Credentials != "" {
		return scrape.Auth{Type: a.Type, Credentials: a.Credentials}
	}
	if b := sc.BasicAuth; b != nil && (b.Username != "" || b.Password != "") {
		return scrape.Auth{BasicUser: b.Username, BasicPass: b.Password, HasBasic: true}
	}
	return scrape.Auth{}
}
```

Update the caller in `run`/`main` (where `mgr.Run` is launched, ~line 102–103):

```go
	scrapeCfgs, err := toScrapeConfigs(cfg)
	if err != nil {
		log.Fatalf("scrape config: %v", err)
	}
	mgr := scrape.NewManager(db, 0)
	go mgr.Run(ctx, scrapeCfgs)
```

(Add `"fmt"` to `main.go` imports if not already present.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/omni/ -run TestToScrapeConfigsCarriesAuthAndTLS -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
gofmt -w ./cmd/omni/
go build ./...
git add cmd/omni/main.go cmd/omni/main_test.go
git -c user.name="pod32g" -c user.email="3311662+pod32g@users.noreply.github.com" commit -m "feat(cmd): wire scrape auth/TLS from config into the manager"
```

---

## Task 9: Full quality gate + binary verification

**Files:** none (verification task)

- [ ] **Step 1: Run the whole gate**

Run: `gofmt -l . ; go vet ./... ; go test ./... -race`
Expected: no gofmt output, vet clean, all tests PASS.

- [ ] **Step 2: Live evidence — bearer + TLS against a throwaway target**

Build and run against a local bearer-protected target to prove a real scrape and a 401→`up=0`. Use a scratch config:

```bash
go build -o /tmp/omni ./cmd/omni
# Terminal A: a target that requires a bearer token
# (use any quick handler; or point at omni's own /metrics for the happy path)
OMNI_TEST_TOKEN=tok /tmp/omni -config examples/omni.yml -storage /tmp/omni-data &
sleep 2
curl -s 'http://127.0.0.1:9090/api/v1/targets' | head
```

Confirm the `omni` self-target shows `up=1`; if you add a bearer-gated target with a wrong token, confirm it shows `up=0` with the 401 recorded in `lastError`. Capture the output in the task notes.

- [ ] **Step 3: Commit (if any gate fixups were needed)**

```bash
git add -A
git -c user.name="pod32g" -c user.email="3311662+pod32g@users.noreply.github.com" commit -m "chore: quality gate fixups for secure scraping" || echo "nothing to commit"
```

---

## Task 10: Documentation

**Files:**
- Modify: `examples/omni.yml`
- Modify: `README.md` (the configuration section)

- [ ] **Step 1: Add a commented secure-scrape example to `examples/omni.yml`**

Append, after the existing `node` example:

```yaml
  # Secure scrape: a target whose /metrics requires a bearer token, over HTTPS.
  # Secrets are never committed — use ${ENV} expansion or *_file references.
  # - job_name: omni-identity
  #   scheme: https
  #   authorization:
  #     credentials: ${OMNI_IDENTITY_TOKEN}   # or credentials_file: /run/secrets/token
  #   tls_config:
  #     ca_file: /etc/ssl/ca.pem              # omit to use system roots
  #     server_name: omni-identity.internal
  #     # insecure_skip_verify: true          # last resort; prefer ca_file
  #   static_configs:
  #     - targets: [omni-identity:8081]
  #
  # Basic auth instead of bearer (mutually exclusive with authorization):
  # - job_name: legacy
  #   basic_auth:
  #     username: scraper
  #     password_file: /run/secrets/pw
  #   static_configs:
  #     - targets: [legacy:9100]
```

- [ ] **Step 2: Document the auth/TLS fields in `README.md`**

In the configuration section, add a short "Secure scraping" subsection describing `scheme`, `authorization` (`credentials` / `credentials_file`), `basic_auth`, `tls_config` (`ca_file`/`cert_file`/`key_file`/`server_name`/`insecure_skip_verify`), the three secret-delivery mechanisms (inline, `${ENV}` with `${VAR:-default}` and fail-loud on unset, and `_file`), and the rotation-needs-restart caveat. Match the surrounding README prose style.

- [ ] **Step 3: Commit**

```bash
git add README.md examples/omni.yml
git -c user.name="pod32g" -c user.email="3311662+pod32g@users.noreply.github.com" commit -m "docs: document secure scrape auth/TLS configuration"
```

---

## Task 11: Live omni-identity wiring (deploy, coordinated)

**Files:**
- Modify: `omni.yml`, `docker-compose.yml`, `.github/workflows/cicd.yml`
- External: chizu (omni-identity deployment env); GitHub repo secret

This task touches deployment and another service. Do the code changes, then coordinate the secret + omni-identity config with the user before the live cutover.

- [ ] **Step 1: Add the env passthrough to `docker-compose.yml`**

In the `environment:` block (after the `LOGSHIP_*` lines):

```yaml
      # omni-identity /metrics bearer token (secret → host env / CI secret).
      OMNI_IDENTITY_TOKEN: ${OMNI_IDENTITY_TOKEN:-}
```

- [ ] **Step 2: Add the bearer credential to `omni.yml`'s omni-identity job**

```yaml
  - job_name: omni-identity
    authorization:
      credentials: ${OMNI_IDENTITY_TOKEN}
    static_configs:
      - targets: [192.168.68.34:8081]
```

(Keep `scheme` http for now — see Step 5 verification. Add `scheme: https` only if omni-identity serves TLS on :8081.)

- [ ] **Step 3: Pass the secret through CI in `.github/workflows/cicd.yml`**

In the `deploy` job's step `env:` block (alongside `SVC`/`VOL`/`PORT`):

```yaml
          OMNI_IDENTITY_TOKEN: ${{ secrets.OMNI_IDENTITY_TOKEN }}
```

- [ ] **Step 4: Provision the shared token (coordinated with the user)**

- Generate: `openssl rand -hex 32`.
- Add it as the GitHub repo secret `OMNI_IDENTITY_TOKEN` (Settings → Secrets → Actions). Ask the user to set this — the value cannot be committed or set programmatically here.
- On chizu, set the same value as `OMNI_METRICS_TOKEN` in the omni-identity deployment env (its host `.env`/secret, gitignored), and restart omni-identity so its `/metrics` requires the token. Locate the omni-identity compose project directory first.

- [ ] **Step 5: Verify on chizu before/after cutover**

SSH `pod32g@100.101.214.34` (Tailscale). Confirm:

```bash
# Before omni-identity is gated: /metrics is open or disabled.
curl -s -o /dev/null -w '%{http_code}\n' http://192.168.68.34:8081/metrics
# After gating: 401 without token, 200 with it.
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer <TOKEN>" http://192.168.68.34:8081/metrics
```

Also confirm whether `:8081` speaks http or https (set `scheme` accordingly). After deploy, check `http://chizu:9090/api/v1/targets` shows the `omni-identity` job `up=1`.

- [ ] **Step 6: Commit**

```bash
git add omni.yml docker-compose.yml .github/workflows/cicd.yml
git -c user.name="pod32g" -c user.email="3311662+pod32g@users.noreply.github.com" commit -m "deploy: authenticate the omni-identity scrape with a bearer token"
```

---

## Self-Review

**Spec coverage:**
- Prometheus schema (scheme/authorization/basic_auth/tls_config) → Task 1.
- `${ENV}` expansion + fail-loud → Task 2, 4.
- Validation (mutual exclusion, both-or-neither, scheme) → Task 3.
- `_file` resolution → Task 4.
- TLS build (CA, mTLS cert/key, server_name, skip-verify) → Task 5, exercised in Task 7.
- Per-job client + bearer/basic header + scheme → Task 6.
- Wiring into the manager → Task 8.
- Quality gate + evidence → Task 9.
- Docs → Task 10.
- Live omni-identity coordination (both repos + token) → Task 11.

**Type consistency:** `scrape.Auth{Type, Credentials, BasicUser, BasicPass, HasBasic}` is used identically in Tasks 6 and 8; `normalizeTarget(job, raw, scheme)` 3-arg signature is consistent across Tasks 6–7 and the updated existing tests; `toScrapeConfigs` returns `([]scrape.ScrapeConfig, error)` in Task 8 and its caller is updated in the same task; `TLSConfig.Build()` defined in Task 5 is called in Task 8.

**Placeholder scan:** Task 10 Step 2 is the one prose-only step (README subsection) — intentionally descriptive since it's documentation matching existing style, not code. All code steps carry complete code.
