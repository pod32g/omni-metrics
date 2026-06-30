package notify

import (
	"testing"
	"time"
)

func TestConfigWithDefaults(t *testing.T) {
	got := Config{}.withDefaults()
	if got.Source != "omni-metrics" {
		t.Errorf("Source = %q, want omni-metrics", got.Source)
	}
	if got.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want 5s", got.Timeout)
	}
	if got.QueueSize != 1024 {
		t.Errorf("QueueSize = %d, want 1024", got.QueueSize)
	}
}

func TestConfigWithDefaultsKeepsExplicit(t *testing.T) {
	in := Config{Source: "custom", Timeout: 2 * time.Second, QueueSize: 8}
	got := in.withDefaults()
	if got.Source != "custom" || got.Timeout != 2*time.Second || got.QueueSize != 8 {
		t.Errorf("withDefaults overrode explicit values: %+v", got)
	}
}
