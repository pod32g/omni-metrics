// Package notify forwards alert state transitions to omni-notify
// (https://github.com/pod32g/omni-notify), a generic event router that delivers
// notifications. The alerting engine is detection-only; this package is the
// outbound delivery seam: a Dispatcher accepts Notifications for firing/resolved
// transitions and POSTs them to omni-notify's POST /api/v1/events endpoint
// (Bearer auth, custom event schema), best-effort with bounded retry.
package notify

import "time"

// Default configuration values applied when a field is left unset.
const (
	defaultSource    = "omni-metrics"
	defaultTimeout   = 5 * time.Second
	defaultQueueSize = 1024
	defaultRetries   = 3
)

// Config configures the outbound notifier. Defaults (see withDefaults) make the
// integration opt-in: a zero Config has Enabled=false.
type Config struct {
	// Enabled turns the integration on. When false no Dispatcher is built.
	Enabled bool
	// URL is the omni-notify base URL, e.g. "http://host:8088". The events path
	// is appended.
	URL string
	// Token is the bearer token presented to omni-notify.
	Token string
	// Source labels every event's "source" field (default "omni-metrics").
	Source string
	// MinSeverity, if set, drops events below this level (one of the known
	// severities). Empty forwards every severity.
	MinSeverity string
	// Timeout bounds each HTTP request (default 5s).
	Timeout time.Duration
	// QueueSize bounds the in-memory send buffer (default 1024). A full buffer
	// drops new notifications (best-effort delivery).
	QueueSize int
	// MaxRetries is the number of retries after the first attempt for retryable
	// failures (default 3; 0 disables retry).
	MaxRetries int
}

// withDefaults returns a copy with operational defaults filled in. It does not
// touch Enabled, URL, Token, MinSeverity, or MaxRetries (0 retries is valid).
func (c Config) withDefaults() Config {
	if c.Source == "" {
		c.Source = defaultSource
	}
	if c.Timeout <= 0 {
		c.Timeout = defaultTimeout
	}
	if c.QueueSize <= 0 {
		c.QueueSize = defaultQueueSize
	}
	return c
}

// Notification is one forwarded alert state transition. The Dispatcher
// canonicalizes Severity (via MapSeverity) and the Client renders it as an
// omni-notify event.
type Notification struct {
	RuleID      string
	RuleName    string
	Fingerprint string // per-series fingerprint within the rule
	Status      string // "firing" or "resolved"
	Severity    string // rule severity (free-form; canonicalized before send)
	Value       float64
	Labels      map[string]string
	Annotations map[string]string
	Time        time.Time
}
