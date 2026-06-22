// Package config loads and validates the YAML configuration for omni-metrics:
// global scrape defaults, storage location/retention, the web listen address,
// and the scrape jobs. A config-less run uses Default(), which scrapes the
// server's own /metrics endpoint.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

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
}

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
	return c, nil
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
