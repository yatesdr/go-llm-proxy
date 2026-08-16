package config

import (
	"fmt"
	"net/url"
)

// BackendConfig describes one upstream server in a logical model or
// processor's replica list. Weight and MaxInflight feed the load balancer;
// Disabled drains a backend without deleting it.
type BackendConfig struct {
	URL         string `yaml:"url"`
	APIKey      string `yaml:"api_key,omitempty"`      // overrides the model's api_key when set
	Weight      int    `yaml:"weight,omitempty"`       // relative selection weight (default 1)
	MaxInflight int    `yaml:"max_inflight,omitempty"` // in-flight requests before spill (0 = unlimited)
	Disabled    bool   `yaml:"disabled,omitempty"`     // drained: excluded from selection
}

// EffectiveBackends returns the replicas serving m. A copy is returned so
// default weights can be normalized without mutating the live config.
func (cfg *Config) EffectiveBackends(m *ModelConfig) []BackendConfig {
	// Runtime-only compatibility for tests and embedders constructing Config
	// values directly. YAML cannot populate these fields; file configs always
	// pass through the migration and use Backends.
	if len(m.Backends) == 0 && m.Backend != "" {
		return []BackendConfig{{URL: m.Backend, APIKey: m.APIKey, Weight: 1}}
	}
	out := make([]BackendConfig, len(m.Backends))
	for i, b := range m.Backends {
		if b.Weight == 0 {
			b.Weight = 1
		}
		out[i] = b
	}
	return out
}

// validateBackendURL enforces the same URL rules as the single-backend field:
// http(s) scheme, a host, and no embedded credentials.
func validateBackendURL(label, backendURL string) error {
	u, err := url.Parse(backendURL)
	if err != nil {
		return fmt.Errorf("%s has invalid backend URL: %w", label, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s backend must use http or https scheme, got %q", label, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%s backend missing host", label)
	}
	if u.User != nil {
		return fmt.Errorf("%s backend must not contain credentials in URL", label)
	}
	return nil
}

func validateBackendList(label string, backends []BackendConfig) error {
	urls := make(map[string]bool, len(backends))
	for _, b := range backends {
		if b.URL == "" {
			return fmt.Errorf("%s has a backend entry missing url", label)
		}
		if err := validateBackendURL(label, b.URL); err != nil {
			return err
		}
		if urls[b.URL] {
			return fmt.Errorf("%s has duplicate backend url %q", label, b.URL)
		}
		urls[b.URL] = true
		if b.Weight < 0 {
			return fmt.Errorf("%s backend %q has negative weight", label, b.URL)
		}
		if b.MaxInflight < 0 {
			return fmt.Errorf("%s backend %q has negative max_inflight", label, b.URL)
		}
	}
	return nil
}
