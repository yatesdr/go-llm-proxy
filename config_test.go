package main

import (
	"strings"
	"testing"
)

func validConfig() *Config {
	return &Config{
		Listen: ":8080",
		Models: []ModelConfig{
			{Name: "test-model", Backend: "http://localhost:8000/v1", Model: "test-model", Timeout: 300},
		},
		Keys: []KeyConfig{
			{Key: "sk-test-key", Name: "admin"},
		},
	}
}

func TestValidateConfig_Valid(t *testing.T) {
	cfg := validConfig()
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidateConfig_NoKeys(t *testing.T) {
	cfg := validConfig()
	cfg.Keys = nil
	// No keys is a warning, not an error.
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("expected no error for empty keys, got: %v", err)
	}
}

func TestValidateConfig_MissingModelName(t *testing.T) {
	cfg := validConfig()
	cfg.Models[0].Name = ""
	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "missing name") {
		t.Fatalf("expected missing name error, got: %v", err)
	}
}

func TestValidateConfig_MissingBackend(t *testing.T) {
	cfg := validConfig()
	cfg.Models[0].Backend = ""
	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "missing backend") {
		t.Fatalf("expected missing backend error, got: %v", err)
	}
}

func TestValidateConfig_InvalidScheme(t *testing.T) {
	cfg := validConfig()
	cfg.Models[0].Backend = "ftp://localhost:8000/v1"
	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "http or https") {
		t.Fatalf("expected scheme error, got: %v", err)
	}
}

func TestValidateConfig_BackendWithCredentials(t *testing.T) {
	cfg := validConfig()
	cfg.Models[0].Backend = "http://user:pass@localhost:8000/v1"
	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("expected credentials error, got: %v", err)
	}
}

func TestValidateConfig_DuplicateModelName(t *testing.T) {
	cfg := validConfig()
	cfg.Models = append(cfg.Models, ModelConfig{
		Name: "test-model", Backend: "http://localhost:8001/v1", Model: "test-model", Timeout: 300,
	})
	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "duplicate model") {
		t.Fatalf("expected duplicate model error, got: %v", err)
	}
}

func TestValidateConfig_DuplicateKey(t *testing.T) {
	cfg := validConfig()
	cfg.Keys = append(cfg.Keys, KeyConfig{Key: "sk-test-key", Name: "dup"})
	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("expected duplicate key error, got: %v", err)
	}
}

func TestValidateConfig_KeyReferencesUnknownModel(t *testing.T) {
	cfg := validConfig()
	cfg.Keys[0].Models = []string{"nonexistent"}
	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("expected unknown model error, got: %v", err)
	}
}

func TestValidateConfig_MissingKeyValue(t *testing.T) {
	cfg := validConfig()
	cfg.Keys[0].Key = ""
	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "missing key value") {
		t.Fatalf("expected missing key error, got: %v", err)
	}
}

func TestValidateConfig_MissingHost(t *testing.T) {
	cfg := validConfig()
	cfg.Models[0].Backend = "http:///v1"
	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "missing host") {
		t.Fatalf("expected missing host error, got: %v", err)
	}
}

func TestValidateConfig_ValidAnthropicType(t *testing.T) {
	cfg := validConfig()
	cfg.Models[0].Type = BackendAnthropic
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("expected no error for anthropic type, got: %v", err)
	}
}

func TestValidateConfig_ValidOpenAIType(t *testing.T) {
	cfg := validConfig()
	cfg.Models[0].Type = BackendOpenAI
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("expected no error for openai type, got: %v", err)
	}
}

func TestValidateConfig_UnknownType(t *testing.T) {
	cfg := validConfig()
	cfg.Models[0].Type = "gemini"
	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "unknown type") {
		t.Fatalf("expected unknown type error, got: %v", err)
	}
}

func TestValidateConfig_ValidMessagesMode(t *testing.T) {
	for _, mode := range []string{"", "auto", "native", "translate"} {
		cfg := validConfig()
		cfg.Models[0].MessagesMode = mode
		if err := validateConfig(cfg); err != nil {
			t.Fatalf("expected no error for messages_mode %q, got: %v", mode, err)
		}
	}
}

func TestValidateConfig_UnknownMessagesMode(t *testing.T) {
	cfg := validConfig()
	cfg.Models[0].MessagesMode = "bogus"
	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "unknown messages_mode") {
		t.Fatalf("expected unknown messages_mode error, got: %v", err)
	}
}

func TestValidateConfig_DashboardRequiresMetrics(t *testing.T) {
	cfg := validConfig()
	cfg.UsageDashboard = true
	cfg.UsageDashboardPassword = "secret"
	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "log_metrics") {
		t.Fatalf("expected log_metrics requirement error, got: %v", err)
	}
}

func TestValidateConfig_DashboardRequiresPassword(t *testing.T) {
	cfg := validConfig()
	cfg.LogMetrics = true
	cfg.UsageDashboard = true
	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "usage_dashboard_password") {
		t.Fatalf("expected password requirement error, got: %v", err)
	}
}

func TestValidateConfig_DashboardValid(t *testing.T) {
	cfg := validConfig()
	cfg.LogMetrics = true
	cfg.UsageDashboard = true
	cfg.UsageDashboardPassword = "a-strong-password"
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("expected no error for valid dashboard config, got: %v", err)
	}
}

func TestValidateConfig_DashboardDisabledNoValidation(t *testing.T) {
	cfg := validConfig()
	cfg.UsageDashboard = false
	cfg.UsageDashboardPassword = ""
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("expected no error when dashboard disabled, got: %v", err)
	}
}
