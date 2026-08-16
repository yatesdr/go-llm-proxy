package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func replicaConfig() *Config {
	return &Config{Listen: ":8080", Models: []ModelConfig{
		{Name: "chat", Backends: []BackendConfig{
			{URL: "http://10.0.0.10:8000/v1", Weight: 2, APIKey: "a"},
			{URL: "http://10.0.0.11:8000/v1", APIKey: "b"},
		}, Model: "chat", Timeout: 300},
	}}
}

func TestValidateConfig_BackendListValid(t *testing.T) {
	if err := validateConfig(replicaConfig()); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestValidateConfig_DuplicateBackendURL(t *testing.T) {
	cfg := replicaConfig()
	cfg.Models[0].Backends[1].URL = cfg.Models[0].Backends[0].URL
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "duplicate backend url") {
		t.Fatalf("expected duplicate URL error, got %v", err)
	}
}

func TestValidateConfig_EmptyBackendList(t *testing.T) {
	cfg := replicaConfig()
	cfg.Models[0].Backends = nil
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "missing backend") {
		t.Fatalf("expected no backends error, got %v", err)
	}
}

func TestEffectiveBackends_NormalizesWeights(t *testing.T) {
	cfg := replicaConfig()
	backends := cfg.EffectiveBackends(&cfg.Models[0])
	if backends[0].Weight != 2 || backends[1].Weight != 1 {
		t.Fatalf("unexpected weights: %+v", backends)
	}
}

func TestFinalizeConfig_BridgesFirstBackend(t *testing.T) {
	cfg := replicaConfig()
	if err := finalizeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Models[0].Backend != "http://10.0.0.10:8000/v1" || cfg.Models[0].APIKey != "a" {
		t.Fatalf("first backend not bridged: %+v", cfg.Models[0])
	}
}

func TestMigrateLegacyPoolsAndSingleBackends(t *testing.T) {
	legacy := []byte(`listen: :8080
pools:
  - name: glm
    backends:
      - url: http://cn3:8000/v1
      - url: http://cn4:8000/v1
        api_key: backend-key
models:
  - name: GLM
    pool: glm
    api_key: shared-key
  - name: Qwen
    backend: http://cn0:8000/v1
    api_key: qwen-key
keys:
  - key: client-key
`)
	out, changed, err := migrateLegacyBackendConfig(legacy)
	if err != nil || !changed {
		t.Fatalf("migration failed: changed=%v err=%v", changed, err)
	}
	text := string(out)
	if strings.Contains(text, "pools:") || strings.Contains(text, "pool: glm") || strings.Contains(text, "    backend:") {
		t.Fatalf("legacy backend schema remained:\n%s", text)
	}
	var cfg Config
	if err := yaml.Unmarshal(out, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Models[0].Backends) != 2 || cfg.Models[0].Backends[0].APIKey != "shared-key" || cfg.Models[0].Backends[1].APIKey != "backend-key" {
		t.Fatalf("pool keys not expanded correctly: %+v", cfg.Models[0].Backends)
	}
	if len(cfg.Models[1].Backends) != 1 || cfg.Models[1].Backends[0].APIKey != "qwen-key" {
		t.Fatalf("single backend not migrated: %+v", cfg.Models[1].Backends)
	}
}

func TestConfigStorePersistsMigrationAndBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	legacy := []byte("models:\n  - name: test\n    backend: http://localhost:8000/v1\nkeys: []\n")
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewConfigStore(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + legacyConfigBackupSuffix); err != nil {
		t.Fatalf("migration backup missing: %v", err)
	}
	current, _ := os.ReadFile(path)
	if !strings.Contains(string(current), "backends:") {
		t.Fatalf("normalized config was not persisted:\n%s", current)
	}
}

func TestEffectiveAdminPassword(t *testing.T) {
	cfg := &Config{}
	if cfg.EffectiveAdminPassword() != "" {
		t.Error("expected empty")
	}
	cfg.UsageDashboard = true
	cfg.UsageDashboardPassword = "dash"
	if cfg.EffectiveAdminPassword() != "dash" {
		t.Error("expected dashboard fallback")
	}
	cfg.AdminPassword = "adm"
	if cfg.EffectiveAdminPassword() != "adm" {
		t.Error("expected admin password")
	}
}
