// Package config loads and validates the YAML configuration for omni-metrics:
// global scrape defaults, storage location/retention, the web listen address,
// and the scrape jobs. A config-less run uses Default(), which scrapes the
// server's own /metrics endpoint.
package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// envRef matches ${VAR} or ${VAR:-default}.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// expandEnv replaces ${VAR} / ${VAR:-default} references. A variable that is
// unset OR empty falls back to its default; ${VAR} with no default that resolves
// empty is an error — fail loud rather than scrape with an empty credential.
// (The deploy path matters: docker-compose's "${VAR:-}" sets the container var
// to "" when a secret is missing, so erroring only on *unset* would silently
// disable auth.) The empty-triggers-default rule matches shell ':-' semantics.
func expandEnv(s string) (string, error) {
	var bad string
	out := envRef.ReplaceAllStringFunc(s, func(m string) string {
		g := envRef.FindStringSubmatch(m)
		name, hasDef, def := g[1], g[2] != "", g[3]
		if v := os.Getenv(name); v != "" {
			return v
		}
		if hasDef {
			return def
		}
		bad = name
		return ""
	})
	if bad != "" {
		return "", fmt.Errorf("environment variable %q is not set or empty", bad)
	}
	return out, nil
}

// DefaultListen is the address the web/API server binds when unspecified. It
// binds loopback only — exposing the server is an explicit choice, not a default.
const DefaultListen = "127.0.0.1:9090"

// Config is the root configuration.
type Config struct {
	Global        GlobalConfig   `yaml:"global"`
	Storage       StorageConfig  `yaml:"storage"`
	Web           WebConfig      `yaml:"web"`
	ScrapeConfigs []ScrapeConfig `yaml:"scrape_configs"`
	Push          PushConfig     `yaml:"push"`
	Alerting      AlertingConfig `yaml:"alerting"`
}

// AlertingConfig configures the built-in alerting engine. Enabled is a *bool so
// an omitted block defaults to enabled (nil) rather than false.
type AlertingConfig struct {
	Enabled           *bool                   `yaml:"enabled"`
	StoragePath       string                  `yaml:"storage_path"`
	DefaultDatasource string                  `yaml:"default_datasource"`
	Datasources       []AlertDatasourceConfig `yaml:"datasources"`
	Notify            NotifyConfig            `yaml:"notify"`
}

// IsEnabled reports whether the alerting engine should run (default true).
func (a AlertingConfig) IsEnabled() bool { return a.Enabled == nil || *a.Enabled }

// NotifyConfig forwards firing/resolved alert transitions to omni-notify. Unlike
// the engine itself, it is opt-in: an omitted block (Enabled nil) is disabled.
type NotifyConfig struct {
	Enabled     *bool    `yaml:"enabled"`
	URL         string   `yaml:"url"`
	Token       string   `yaml:"token"`
	Source      string   `yaml:"source"`
	MinSeverity string   `yaml:"min_severity"`
	Timeout     Duration `yaml:"timeout"`
	QueueSize   int      `yaml:"queue_size"`
	// MaxRetries is a *int so an explicit 0 (no retry) is distinguishable from
	// unset (defaults to DefaultNotifyMaxRetries).
	MaxRetries *int `yaml:"max_retries"`
}

// IsEnabled reports whether outbound notification is on (default false).
func (n NotifyConfig) IsEnabled() bool { return n.Enabled != nil && *n.Enabled }

// Notify defaults.
const (
	DefaultNotifyTimeout    = 5 * time.Second
	DefaultNotifyQueueSize  = 1024
	DefaultNotifyMaxRetries = 3
	defaultNotifySource     = "omni-metrics"
)

// AlertDatasourceConfig is one config-defined alert datasource. It reuses the
// scrape auth shapes (Authorization / BasicAuth) for credential resolution.
type AlertDatasourceConfig struct {
	Name          string            `yaml:"name"`
	Type          string            `yaml:"type"`
	URL           string            `yaml:"url"`
	Timeout       Duration          `yaml:"timeout"`
	Authorization *Authorization    `yaml:"authorization"`
	BasicAuth     *BasicAuth        `yaml:"basic_auth"`
	Headers       map[string]string `yaml:"headers"`
	Enabled       *bool             `yaml:"enabled"`
}

// IsEnabled reports whether the datasource is enabled (default true).
func (d AlertDatasourceConfig) IsEnabled() bool { return d.Enabled == nil || *d.Enabled }

// DefaultAlertDatasourceTimeout is applied when a datasource omits a timeout.
const DefaultAlertDatasourceTimeout = 30 * time.Second

// GlobalConfig holds defaults applied to scrape jobs that omit their own.
type GlobalConfig struct {
	ScrapeInterval Duration `yaml:"scrape_interval"`
	ScrapeTimeout  Duration `yaml:"scrape_timeout"`
}

// StorageConfig configures the on-disk WAL/data directory and head retention.
type StorageConfig struct {
	Path      string   `yaml:"path"`
	Retention Duration `yaml:"retention"`
}

// WebConfig configures the HTTP server.
type WebConfig struct {
	Listen string `yaml:"listen"`
}

// PushConfig configures the JSON push-ingestion endpoint. Enabled is a *bool so
// an omitted block defaults to enabled (nil) rather than false.
type PushConfig struct {
	Enabled      *bool  `yaml:"enabled"`
	SampleLimit  int    `yaml:"sample_limit"`
	MaxBodyBytes int64  `yaml:"max_body_bytes"`
	AuthToken    string `yaml:"auth_token"`
}

// DefaultPushBodyBytes bounds a push request body (16 MiB).
const DefaultPushBodyBytes = 16 << 20

// IsEnabled reports whether the push endpoint should be served (default true).
func (p PushConfig) IsEnabled() bool { return p.Enabled == nil || *p.Enabled }

// BodyLimit returns the configured request-body cap, or the default when unset.
func (p PushConfig) BodyLimit() int64 {
	if p.MaxBodyBytes > 0 {
		return p.MaxBodyBytes
	}
	return DefaultPushBodyBytes
}

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

// StaticConfig is a static list of target addresses.
type StaticConfig struct {
	Targets []string `yaml:"targets"`
}

// Default returns a configuration that scrapes only the server's own /metrics.
func Default() *Config {
	c := &Config{
		Global:  GlobalConfig{ScrapeInterval: Duration(15 * time.Second), ScrapeTimeout: Duration(10 * time.Second)},
		Storage: StorageConfig{Path: "", Retention: Duration(6 * time.Hour)},
		Web:     WebConfig{Listen: DefaultListen},
	}
	c.ScrapeConfigs = []ScrapeConfig{{
		JobName:       "omni",
		StaticConfigs: []StaticConfig{{Targets: []string{c.Web.Listen}}},
	}}
	return c
}

// Load reads and validates a configuration file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	return LoadBytes(b)
}

// LoadBytes parses, defaults, and validates a configuration document.
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
	for i := range c.Alerting.Datasources {
		ds := &c.Alerting.Datasources[i]
		if a := ds.Authorization; a != nil {
			v, err := resolveSecret("authorization.credentials", a.Credentials, a.CredentialsFile)
			if err != nil {
				return fmt.Errorf("alerting datasource %q: %w", ds.Name, err)
			}
			a.Credentials = v
		}
		if b := ds.BasicAuth; b != nil {
			u, err := resolveSecret("basic_auth.username", b.Username, b.UsernameFile)
			if err != nil {
				return fmt.Errorf("alerting datasource %q: %w", ds.Name, err)
			}
			p, err := resolveSecret("basic_auth.password", b.Password, b.PasswordFile)
			if err != nil {
				return fmt.Errorf("alerting datasource %q: %w", ds.Name, err)
			}
			b.Username, b.Password = u, p
		}
	}
	if n := &c.Alerting.Notify; n.IsEnabled() && n.Token != "" {
		v, err := expandEnv(n.Token)
		if err != nil {
			return fmt.Errorf("alerting notify token: %w", err)
		}
		n.Token = v
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

func (c *Config) applyDefaults() {
	if c.Global.ScrapeInterval == 0 {
		c.Global.ScrapeInterval = Duration(15 * time.Second)
	}
	if c.Global.ScrapeTimeout == 0 {
		c.Global.ScrapeTimeout = Duration(10 * time.Second)
	}
	if c.Storage.Retention == 0 {
		c.Storage.Retention = Duration(6 * time.Hour)
	}
	if c.Web.Listen == "" {
		c.Web.Listen = DefaultListen
	}
	if len(c.ScrapeConfigs) == 0 {
		c.ScrapeConfigs = []ScrapeConfig{{
			JobName:       "omni",
			StaticConfigs: []StaticConfig{{Targets: []string{c.Web.Listen}}},
		}}
	}
	for i := range c.ScrapeConfigs {
		sc := &c.ScrapeConfigs[i]
		if sc.ScrapeInterval == 0 {
			sc.ScrapeInterval = c.Global.ScrapeInterval
		}
		if sc.ScrapeTimeout == 0 {
			sc.ScrapeTimeout = c.Global.ScrapeTimeout
		}
	}
	for i := range c.Alerting.Datasources {
		ds := &c.Alerting.Datasources[i]
		if ds.Type == "" {
			ds.Type = "prometheus"
		}
		if ds.Timeout == 0 {
			ds.Timeout = Duration(DefaultAlertDatasourceTimeout)
		}
	}
	if n := &c.Alerting.Notify; n.IsEnabled() {
		if n.Source == "" {
			n.Source = defaultNotifySource
		}
		if n.Timeout == 0 {
			n.Timeout = Duration(DefaultNotifyTimeout)
		}
		if n.QueueSize == 0 {
			n.QueueSize = DefaultNotifyQueueSize
		}
		if n.MaxRetries == nil {
			d := DefaultNotifyMaxRetries
			n.MaxRetries = &d
		}
	}
}

func (c *Config) validate() error {
	seen := map[string]bool{}
	for _, sc := range c.ScrapeConfigs {
		if sc.JobName == "" {
			return fmt.Errorf("scrape config: job_name must not be empty")
		}
		if seen[sc.JobName] {
			return fmt.Errorf("scrape config: duplicate job_name %q", sc.JobName)
		}
		seen[sc.JobName] = true
		total := 0
		for _, s := range sc.StaticConfigs {
			total += len(s.Targets)
		}
		if total == 0 {
			return fmt.Errorf("scrape config %q: at least one target required", sc.JobName)
		}
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
	}
	if err := c.validateAlerting(); err != nil {
		return err
	}
	return nil
}

func (c *Config) validateAlerting() error {
	seen := map[string]bool{}
	for _, ds := range c.Alerting.Datasources {
		if ds.Name == "" {
			return fmt.Errorf("alerting datasource: name must not be empty")
		}
		if seen[ds.Name] {
			return fmt.Errorf("alerting datasource: duplicate name %q", ds.Name)
		}
		seen[ds.Name] = true
		if ds.URL == "" {
			return fmt.Errorf("alerting datasource %q: url is required", ds.Name)
		}
		if ds.Authorization != nil && ds.BasicAuth != nil {
			return fmt.Errorf("alerting datasource %q: authorization and basic_auth are mutually exclusive", ds.Name)
		}
	}
	if d := c.Alerting.DefaultDatasource; d != "" && len(c.Alerting.Datasources) > 0 && !seen[d] {
		return fmt.Errorf("alerting: default_datasource %q is not a configured datasource", d)
	}
	if err := c.Alerting.Notify.validate(); err != nil {
		return err
	}
	return nil
}

// validate checks an enabled notify block. URL and token are required; the URL
// must be http(s) with a host; min_severity, if set, must be a known level. The
// token is validated only for presence here — its ${VAR} expansion (and
// fail-loud-on-unset behavior) happens later in resolveSecrets.
func (n NotifyConfig) validate() error {
	if !n.IsEnabled() {
		return nil
	}
	if n.URL == "" {
		return fmt.Errorf("alerting notify: url is required when enabled")
	}
	u, err := url.Parse(n.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("alerting notify: url %q must be an http(s) URL with a host", n.URL)
	}
	if u.User != nil {
		return fmt.Errorf("alerting notify: url must not embed userinfo; use token for authentication")
	}
	if n.Token == "" {
		return fmt.Errorf("alerting notify: token is required when enabled")
	}
	switch n.MinSeverity {
	case "", "critical", "error", "warning", "info", "debug":
	default:
		return fmt.Errorf("alerting notify: min_severity %q must be one of critical|error|warning|info|debug", n.MinSeverity)
	}
	return nil
}

// AllTargets returns the flattened target list for a job.
func (sc ScrapeConfig) AllTargets() []string {
	var out []string
	for _, s := range sc.StaticConfigs {
		out = append(out, s.Targets...)
	}
	return out
}

// Duration is a time.Duration that (un)marshals from Prometheus-style strings
// such as "15s", "5m", "1h", "2d", "1w".
type Duration time.Duration

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := parseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalYAML() (interface{}, error) {
	return time.Duration(d).String(), nil
}

// parseDuration parses compound durations with units s, m, h, d, w, y.
func parseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	var total time.Duration
	i := 0
	for i < len(s) {
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if start == i || i >= len(s) {
			return 0, fmt.Errorf("malformed duration %q", s)
		}
		num, _ := strconv.Atoi(s[start:i])
		var unit time.Duration
		switch s[i] {
		case 's':
			unit = time.Second
		case 'm':
			unit = time.Minute
		case 'h':
			unit = time.Hour
		case 'd':
			unit = 24 * time.Hour
		case 'w':
			unit = 7 * 24 * time.Hour
		case 'y':
			unit = 365 * 24 * time.Hour
		default:
			return 0, fmt.Errorf("unknown duration unit %q in %q", string(s[i]), s)
		}
		total += time.Duration(num) * unit
		i++
	}
	return total, nil
}
