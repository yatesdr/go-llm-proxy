package config

import (
	"strings"
	"testing"
)

func poolConfig() *Config {
	return &Config{
		Listen: ":8080",
		Pools: []PoolConfig{
			{Name: "cluster", Backends: []BackendConfig{
				{URL: "http://10.0.0.10:8000/v1", Weight: 2},
				{URL: "http://10.0.0.11:8000/v1", APIKey: "backend-key"},
			}},
		},
		Models: []ModelConfig{
			{Name: "pooled", Pool: "cluster", APIKey: "model-key", Model: "pooled", Timeout: 300},
			{Name: "inline", Backends: []BackendConfig{{URL: "http://10.0.0.20:8000/v1"}}, Model: "inline", Timeout: 300},
			{Name: "single", Backend: "http://localhost:8000/v1", Model: "single", Timeout: 300},
		},
	}
}

func TestValidateConfig_PoolValid(t *testing.T) {
	if err := validateConfig(poolConfig()); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidateConfig_UnknownPool(t *testing.T) {
	cfg := poolConfig()
	cfg.Models[0].Pool = "nope"
	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "unknown pool") {
		t.Fatalf("expected unknown pool error, got: %v", err)
	}
}

func TestValidateConfig_MultipleBackendSources(t *testing.T) {
	cfg := poolConfig()
	cfg.Models[0].Backend = "http://localhost:9000/v1"
	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "only one of") {
		t.Fatalf("expected multiple-source error, got: %v", err)
	}
}

func TestValidateConfig_DuplicatePoolName(t *testing.T) {
	cfg := poolConfig()
	cfg.Pools = append(cfg.Pools, cfg.Pools[0])
	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "duplicate pool name") {
		t.Fatalf("expected duplicate pool error, got: %v", err)
	}
}

func TestValidateConfig_DuplicateBackendURLInPool(t *testing.T) {
	cfg := poolConfig()
	cfg.Pools[0].Backends[1].URL = cfg.Pools[0].Backends[0].URL
	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "duplicate backend url") {
		t.Fatalf("expected duplicate url error, got: %v", err)
	}
}

func TestValidateConfig_EmptyPool(t *testing.T) {
	cfg := poolConfig()
	cfg.Pools[0].Backends = nil
	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "no backends") {
		t.Fatalf("expected empty pool error, got: %v", err)
	}
}

func TestValidateConfig_BedrockRejectsPool(t *testing.T) {
	cfg := poolConfig()
	cfg.Models[0].Type = BackendBedrock
	cfg.Models[0].Region = "us-east-1"
	cfg.Models[0].APIKey = "bedrock-key"
	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "does not support backends/pool") {
		t.Fatalf("expected bedrock pool rejection, got: %v", err)
	}
}

func TestEffectiveBackends_PoolResolvesKeysAndWeights(t *testing.T) {
	cfg := poolConfig()
	backends := cfg.EffectiveBackends(&cfg.Models[0])
	if len(backends) != 2 {
		t.Fatalf("expected 2 backends, got %d", len(backends))
	}
	// Backend without its own key inherits the model key; explicit key wins.
	if backends[0].APIKey != "model-key" {
		t.Errorf("expected inherited model key, got %q", backends[0].APIKey)
	}
	if backends[1].APIKey != "backend-key" {
		t.Errorf("expected backend key override, got %q", backends[1].APIKey)
	}
	// Zero weight normalizes to 1; explicit weight preserved.
	if backends[0].Weight != 2 || backends[1].Weight != 1 {
		t.Errorf("unexpected weights: %d, %d", backends[0].Weight, backends[1].Weight)
	}
}

func TestEffectiveBackends_SingleBackend(t *testing.T) {
	cfg := poolConfig()
	backends := cfg.EffectiveBackends(&cfg.Models[2])
	if len(backends) != 1 || backends[0].URL != "http://localhost:8000/v1" {
		t.Fatalf("unexpected backends: %+v", backends)
	}
}

func TestFinalizeConfig_BridgesLegacyBackendField(t *testing.T) {
	cfg := poolConfig()
	if err := finalizeConfig(cfg); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if cfg.Models[0].Backend != "http://10.0.0.10:8000/v1" {
		t.Errorf("pooled model backend not bridged: %q", cfg.Models[0].Backend)
	}
	if cfg.Models[1].Backend != "http://10.0.0.20:8000/v1" {
		t.Errorf("inline model backend not bridged: %q", cfg.Models[1].Backend)
	}
}

func TestEffectiveAdminPassword(t *testing.T) {
	cfg := &Config{}
	if cfg.EffectiveAdminPassword() != "" {
		t.Error("expected empty when nothing configured")
	}
	cfg.UsageDashboard = true
	cfg.UsageDashboardPassword = "dash"
	if cfg.EffectiveAdminPassword() != "dash" {
		t.Error("expected dashboard password fallback")
	}
	cfg.AdminPassword = "adm"
	if cfg.EffectiveAdminPassword() != "adm" {
		t.Error("expected admin_password to win")
	}
}
