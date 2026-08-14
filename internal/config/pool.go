package config

import (
	"fmt"
	"net/url"
)

// BackendConfig describes one upstream server inside a pool (or a model's
// inline backend list). Weight and MaxInflight feed the load balancer;
// Disabled drains a backend without deleting it.
type BackendConfig struct {
	URL         string `yaml:"url"`
	APIKey      string `yaml:"api_key,omitempty"`      // overrides the model's api_key when set
	Weight      int    `yaml:"weight,omitempty"`       // relative selection weight (default 1)
	MaxInflight int    `yaml:"max_inflight,omitempty"` // in-flight requests before spill (0 = unlimited)
	Disabled    bool   `yaml:"disabled,omitempty"`     // drained: excluded from selection
}

// PoolConfig is a named group of backends that one or more models share.
// Health, in-flight counts, and breaker state are tracked per backend URL,
// so models referencing the same pool balance against the same real capacity.
type PoolConfig struct {
	Name     string          `yaml:"name"`
	Backends []BackendConfig `yaml:"backends"`
}

// FindPool returns the PoolConfig with the given name, or nil if not found.
func FindPool(cfg *Config, name string) *PoolConfig {
	for i := range cfg.Pools {
		if cfg.Pools[i].Name == name {
			return &cfg.Pools[i]
		}
	}
	return nil
}

// EffectiveBackends returns the list of backends serving m: the referenced
// pool's backends, the model's inline list, or the single backend field as a
// one-element list. Each returned entry has APIKey resolved (backend override
// falling back to the model's api_key), so callers never consult m.APIKey.
func (cfg *Config) EffectiveBackends(m *ModelConfig) []BackendConfig {
	var src []BackendConfig
	switch {
	case m.Pool != "":
		if p := FindPool(cfg, m.Pool); p != nil {
			src = p.Backends
		}
	case len(m.Backends) > 0:
		src = m.Backends
	default:
		return []BackendConfig{{URL: m.Backend, APIKey: m.APIKey, Weight: 1}}
	}

	out := make([]BackendConfig, len(src))
	for i, b := range src {
		if b.APIKey == "" {
			b.APIKey = m.APIKey
		}
		if b.Weight == 0 {
			b.Weight = 1
		}
		out[i] = b
	}
	return out
}

// PoolReferrers returns the names of models that reference the named pool.
func PoolReferrers(cfg *Config, poolName string) []string {
	var refs []string
	for _, m := range cfg.Models {
		if m.Pool == poolName {
			refs = append(refs, m.Name)
		}
	}
	return refs
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

// validatePools checks pool definitions: unique non-empty names, at least one
// backend per pool, valid backend URLs, no duplicate URLs within a pool, and
// sane weight / max_inflight values.
func validatePools(cfg *Config) error {
	poolNames := make(map[string]bool, len(cfg.Pools))
	for _, p := range cfg.Pools {
		if p.Name == "" {
			return fmt.Errorf("pool entry missing name")
		}
		if poolNames[p.Name] {
			return fmt.Errorf("duplicate pool name %q", p.Name)
		}
		poolNames[p.Name] = true

		if len(p.Backends) == 0 {
			return fmt.Errorf("pool %q has no backends", p.Name)
		}
		if err := validateBackendList("pool "+p.Name, p.Backends); err != nil {
			return err
		}
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
