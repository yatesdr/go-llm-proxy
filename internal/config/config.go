package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

type ProcessorsConfig struct {
	Vision       string `yaml:"vision"`         // model name for vision processing (empty = disabled)
	OCR          string `yaml:"ocr"`            // model name for OCR/text extraction from PDF page images (falls back to vision)
	WebSearchKey string `yaml:"web_search_key"` // Tavily API key (empty = web search disabled)
}

type Config struct {
	Listen                 string           `yaml:"listen"`
	Models                 []ModelConfig    `yaml:"models"`
	Keys                   []KeyConfig      `yaml:"keys"`
	Services               ServicesConfig   `yaml:"services"`                 // external service proxies (Qdrant, etc.)
	Processors             ProcessorsConfig `yaml:"processors"`               // global processor defaults
	TrustedProxies         []string         `yaml:"trusted_proxies"`          // CIDR or IPs allowed to set X-Real-IP
	ServeConfigGenerator   bool             `yaml:"serve_config_generator"`   // enable the config generator page at GET /
	LogMetrics             bool             `yaml:"log_metrics"`              // enable per-request usage logging to SQLite
	UsageDB                string           `yaml:"usage_db"`                 // path to SQLite usage database (default: usage.db)
	UsageDashboard         bool             `yaml:"usage_dashboard"`          // enable the usage dashboard at /usage
	UsageDashboardPassword string           `yaml:"usage_dashboard_password"` // password for the usage dashboard
}

const (
	BackendOpenAI    = "openai"
	BackendAnthropic = "anthropic"
	BackendBedrock   = "bedrock"

	ResponsesModeAuto      = ""          // default: probe backend, cache result
	ResponsesModeNative    = "native"    // always passthrough
	ResponsesModeTranslate = "translate" // always translate to Chat Completions

	MessagesModeAuto      = ""          // default: anthropic backends passthrough, others translate
	MessagesModeNative    = "native"    // always passthrough (force Anthropic protocol to backend)
	MessagesModeTranslate = "translate" // always translate Anthropic Messages to Chat Completions
)

// SamplingDefaults contains default sampling parameters for a model.
// These are injected into requests that don't specify them.
type SamplingDefaults struct {
	Temperature      *float64 `yaml:"temperature"`       // controls randomness (0.0 = deterministic)
	TopP             *float64 `yaml:"top_p"`             // nucleus sampling threshold
	TopK             *int     `yaml:"top_k"`             // limits vocabulary to top K tokens
	MaxNewTokens     *int     `yaml:"max_new_tokens"`    // maximum tokens to generate (maps to max_tokens)
	FrequencyPenalty *float64 `yaml:"frequency_penalty"` // penalizes repeated tokens by frequency (0.0–2.0)
	PresencePenalty  *float64 `yaml:"presence_penalty"`  // penalizes tokens that have appeared at all (0.0–2.0)
	ReasoningEffort  *string  `yaml:"reasoning_effort"`  // thinking budget: low, medium, or high
	Stop             []string `yaml:"stop"`              // strings that trigger end of generation
}

type ModelConfig struct {
	Name           string            `yaml:"name"`
	Backend        string            `yaml:"backend"`          // upstream URL e.g. http://192.168.100.10:8000/v1
	APIKey         string            `yaml:"api_key"`          // key to send to the backend (if required)
	Model          string            `yaml:"model"`            // model name to send to the backend (if different from Name)
	Timeout        int               `yaml:"timeout"`          // request timeout in seconds (default 300)
	Type           string            `yaml:"type"`             // backend type: "" or "openai" (default), "anthropic"
	ResponsesMode  string            `yaml:"responses_mode"`   // "auto" (default), "native", or "translate"
	MessagesMode   string            `yaml:"messages_mode"`    // "auto" (default), "native", or "translate"
	ContextWindow  int               `yaml:"context_window"`   // max context tokens (0 = auto-detect from backend)
	SupportsVision bool              `yaml:"supports_vision"`  // model handles images natively
	ForcePipeline  bool              `yaml:"force_pipeline"`   // run pipeline even on native backends
	Processors     *ProcessorsConfig `yaml:"processors"`       // per-model processor overrides (nil = use global)
	Defaults       *SamplingDefaults `yaml:"defaults"`         // default sampling parameters (nil = use backend defaults)

	// AWS Bedrock fields (only used when type: "bedrock").
	// If api_key is set, it is sent as a Bedrock API key bearer token and the
	// SigV4 fields below are ignored. Otherwise SigV4 signing is used with the
	// provided IAM credentials, falling back to AWS_ACCESS_KEY_ID /
	// AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN environment variables.
	Region          string `yaml:"region"`            // AWS region, e.g. "us-east-1"
	AWSAccessKey    string `yaml:"aws_access_key"`    // IAM access key ID (AKIA...)
	AWSSecretKey    string `yaml:"aws_secret_key"`    // IAM secret access key
	AWSSessionToken string `yaml:"aws_session_token"` // optional STS session token
}

type KeyConfig struct {
	Key    string   `yaml:"key"`
	Name   string   `yaml:"name"`   // friendly name for logging
	Models []string `yaml:"models"` // allowed models, empty = all
}

// ServicesConfig contains configuration for external services proxied by the server.
type ServicesConfig struct {
	Qdrant *QdrantConfig `yaml:"qdrant"`
}

// QdrantConfig configures the Qdrant vector database proxy.
type QdrantConfig struct {
	Backend string         `yaml:"backend"` // Qdrant server URL e.g. http://192.168.5.143:6333
	APIKey  string         `yaml:"api_key"` // API key to send to Qdrant backend
	AppKeys []AppKeyConfig `yaml:"app_keys"`
}

// AppKeyConfig defines an application key for service access.
type AppKeyConfig struct {
	Name string `yaml:"name"` // friendly name for logging
	Key  string `yaml:"key"`  // the actual API key
}

// ConfigStore provides thread-safe access to the current config.
type ConfigStore struct {
	mu       sync.RWMutex
	config   *Config
	path     string
	onReload func(*Config) // called after each successful reload (optional)
}

func NewConfigStore(path string) (*ConfigStore, error) {
	cs := &ConfigStore{path: path}
	if err := cs.Load(); err != nil {
		return nil, err
	}
	return cs, nil
}

// NewTestConfigStore creates a ConfigStore from an in-memory Config (for testing).
func NewTestConfigStore(cfg *Config) *ConfigStore {
	return &ConfigStore{config: cfg}
}

func (cs *ConfigStore) Load() error {
	data, err := os.ReadFile(cs.path)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}

	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}

	for i := range cfg.Models {
		m := &cfg.Models[i]
		if m.Timeout == 0 {
			m.Timeout = 300
		}
		if m.Model == "" {
			m.Model = m.Name
		}
		if m.Type == BackendBedrock {
			applyBedrockDefaults(m)
		}
	}

	if err := validateConfig(&cfg); err != nil {
		return err
	}

	cs.mu.Lock()
	cs.config = &cfg
	cs.mu.Unlock()

	slog.Info("config loaded", "models", len(cfg.Models), "keys", len(cfg.Keys))

	if cs.onReload != nil {
		cs.onReload(&cfg)
	}
	return nil
}

// SetOnReload registers a callback invoked after each successful config reload.
func (cs *ConfigStore) SetOnReload(fn func(*Config)) {
	cs.onReload = fn
}

func (cs *ConfigStore) Get() *Config {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.config
}

// Watch starts a goroutine that reloads config when the file changes on disk.
// Watches the parent directory to survive rename-based saves (vim, etc.).
// Returns a stop function.
func (cs *ConfigStore) Watch() (stop func(), err error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("creating file watcher: %w", err)
	}

	absPath, err := filepath.Abs(cs.path)
	if err != nil {
		watcher.Close()
		return nil, fmt.Errorf("resolving config path: %w", err)
	}
	dir := filepath.Dir(absPath)
	base := filepath.Base(absPath)

	if err := watcher.Add(dir); err != nil {
		watcher.Close()
		return nil, fmt.Errorf("watching %s: %w", dir, err)
	}

	go func() {
		// Debounce: editors often write a temp file then rename, producing
		// multiple events in quick succession. Wait briefly before reloading.
		var debounce *time.Timer
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				// Only react to events on our config file.
				if filepath.Base(event.Name) != base {
					continue
				}
				if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
					continue
				}
				if debounce != nil {
					debounce.Stop()
				}
				debounce = time.AfterFunc(500*time.Millisecond, func() {
					slog.Info("config file changed, reloading")
					if err := cs.Load(); err != nil {
						slog.Error("failed to reload config", "error", err)
					}
				})
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				slog.Error("file watcher error", "error", err)
			}
		}
	}()

	slog.Info("watching config file for changes", "path", absPath)
	return func() { watcher.Close() }, nil
}

// applyBedrockDefaults fills in the default Bedrock backend URL and pulls AWS
// credentials from the environment when not explicitly configured. Called for
// every model with type: "bedrock" during config load.
func applyBedrockDefaults(m *ModelConfig) {
	if m.Backend == "" && m.Region != "" {
		m.Backend = fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com", m.Region)
	}
	// API-key auth shortcuts SigV4 entirely; only fall back to env for IAM keys.
	if m.APIKey != "" {
		return
	}
	if m.AWSAccessKey == "" {
		m.AWSAccessKey = os.Getenv("AWS_ACCESS_KEY_ID")
	}
	if m.AWSSecretKey == "" {
		m.AWSSecretKey = os.Getenv("AWS_SECRET_ACCESS_KEY")
	}
	if m.AWSSessionToken == "" {
		m.AWSSessionToken = os.Getenv("AWS_SESSION_TOKEN")
	}
}

// FindModel returns the ModelConfig with the given name, or nil if not found.
func FindModel(cfg *Config, name string) *ModelConfig {
	for i := range cfg.Models {
		if cfg.Models[i].Name == name {
			return &cfg.Models[i]
		}
	}
	return nil
}


func validateConfig(cfg *Config) error {
	if len(cfg.Keys) == 0 {
		slog.Warn("no API keys configured — all requests will be unauthenticated")
	}

	if cfg.UsageDashboard {
		if !cfg.LogMetrics {
			return fmt.Errorf("usage_dashboard requires log_metrics to be enabled")
		}
		if cfg.UsageDashboardPassword == "" {
			return fmt.Errorf("usage_dashboard requires usage_dashboard_password to be set")
		}
	}

	names := make(map[string]bool)
	for _, m := range cfg.Models {
		if m.Name == "" {
			return fmt.Errorf("model entry missing name")
		}
		if m.Backend == "" {
			return fmt.Errorf("model %q missing backend", m.Name)
		}

		// Validate backend URL.
		u, err := url.Parse(m.Backend)
		if err != nil {
			return fmt.Errorf("model %q has invalid backend URL: %w", m.Name, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("model %q backend must use http or https scheme, got %q", m.Name, u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("model %q backend missing host", m.Name)
		}
		if u.User != nil {
			return fmt.Errorf("model %q backend must not contain credentials in URL", m.Name)
		}

		switch m.Type {
		case "", BackendOpenAI, BackendAnthropic:
		case BackendBedrock:
			if m.Region == "" {
				return fmt.Errorf("model %q (bedrock) requires region", m.Name)
			}
			if m.APIKey == "" && (m.AWSAccessKey == "" || m.AWSSecretKey == "") {
				return fmt.Errorf("model %q (bedrock) requires either api_key (Bedrock API key) or aws_access_key + aws_secret_key (set in config or environment)", m.Name)
			}
		default:
			return fmt.Errorf("model %q has unknown type %q (must be %q, %q, or %q)", m.Name, m.Type, BackendOpenAI, BackendAnthropic, BackendBedrock)
		}

		switch m.ResponsesMode {
		case "", "auto", ResponsesModeNative, ResponsesModeTranslate:
		default:
			return fmt.Errorf("model %q has unknown responses_mode %q (must be %q, %q, or omitted)", m.Name, m.ResponsesMode, ResponsesModeNative, ResponsesModeTranslate)
		}

		switch m.MessagesMode {
		case "", "auto", MessagesModeNative, MessagesModeTranslate:
		default:
			return fmt.Errorf("model %q has unknown messages_mode %q (must be %q, %q, or omitted)", m.Name, m.MessagesMode, MessagesModeNative, MessagesModeTranslate)
		}

		if d := m.Defaults; d != nil && d.ReasoningEffort != nil {
			switch *d.ReasoningEffort {
			case "low", "medium", "high":
			default:
				return fmt.Errorf("model %q has unknown reasoning_effort %q (must be low, medium, or high)", m.Name, *d.ReasoningEffort)
			}
		}

		if names[m.Name] {
			return fmt.Errorf("duplicate model name %q", m.Name)
		}
		names[m.Name] = true
	}

	// Validate global vision processor references a defined model.
	if v := cfg.Processors.Vision; v != "" {
		if !names[v] {
			return fmt.Errorf("global processors.vision references unknown model %q", v)
		}
	}

	// Validate global OCR processor references a defined model.
	if v := cfg.Processors.OCR; v != "" && v != "none" {
		if !names[v] {
			return fmt.Errorf("global processors.ocr references unknown model %q", v)
		}
	}

	// Validate per-model processor overrides reference defined models.
	for _, m := range cfg.Models {
		if m.Processors != nil && m.Processors.Vision != "" && m.Processors.Vision != "none" {
			if !names[m.Processors.Vision] {
				return fmt.Errorf("model %q processors.vision references unknown model %q", m.Name, m.Processors.Vision)
			}
		}
		if m.Processors != nil && m.Processors.OCR != "" && m.Processors.OCR != "none" {
			if !names[m.Processors.OCR] {
				return fmt.Errorf("model %q processors.ocr references unknown model %q", m.Name, m.Processors.OCR)
			}
		}
	}

	// Auto-infer SupportsVision: any model referenced as a vision processor
	// obviously supports vision — don't require the user to say so twice.
	visionModels := make(map[string]bool)
	if cfg.Processors.Vision != "" {
		visionModels[cfg.Processors.Vision] = true
	}
	for _, m := range cfg.Models {
		if m.Processors != nil && m.Processors.Vision != "" && m.Processors.Vision != "none" {
			visionModels[m.Processors.Vision] = true
		}
	}
	for i := range cfg.Models {
		if visionModels[cfg.Models[i].Name] && !cfg.Models[i].SupportsVision {
			cfg.Models[i].SupportsVision = true
		}
	}

	keys := make(map[string]bool)
	for _, k := range cfg.Keys {
		if k.Key == "" {
			return fmt.Errorf("key entry missing key value")
		}
		if keys[k.Key] {
			return fmt.Errorf("duplicate key for %q", k.Name)
		}
		keys[k.Key] = true

		for _, m := range k.Models {
			if !names[m] {
				return fmt.Errorf("key %q references unknown model %q", k.Name, m)
			}
		}
	}

	// Validate Qdrant service config.
	if q := cfg.Services.Qdrant; q != nil {
		if q.Backend == "" {
			return fmt.Errorf("services.qdrant missing backend")
		}
		u, err := url.Parse(q.Backend)
		if err != nil {
			return fmt.Errorf("services.qdrant has invalid backend URL: %w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("services.qdrant backend must use http or https scheme, got %q", u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("services.qdrant backend missing host")
		}

		appKeys := make(map[string]bool)
		for _, ak := range q.AppKeys {
			if ak.Key == "" {
				return fmt.Errorf("services.qdrant app_key entry missing key value")
			}
			if ak.Name == "" {
				return fmt.Errorf("services.qdrant app_key entry missing name")
			}
			if appKeys[ak.Key] {
				return fmt.Errorf("services.qdrant duplicate app_key for %q", ak.Name)
			}
			appKeys[ak.Key] = true
		}
	}

	return nil
}

// ApplySamplingDefaults injects default sampling parameters into a Chat Completions
// request map. Only sets values that are not already present in the request.
// This allows per-model defaults to be applied without overriding explicit user values.
func (m *ModelConfig) ApplySamplingDefaults(chatReq map[string]any) {
	if m.Defaults == nil {
		return
	}
	d := m.Defaults
	var applied []string

	if d.Temperature != nil {
		if _, exists := chatReq["temperature"]; !exists {
			chatReq["temperature"] = *d.Temperature
			applied = append(applied, fmt.Sprintf("temperature=%.2f", *d.Temperature))
		}
	}
	if d.TopP != nil {
		if _, exists := chatReq["top_p"]; !exists {
			chatReq["top_p"] = *d.TopP
			applied = append(applied, fmt.Sprintf("top_p=%.2f", *d.TopP))
		}
	}
	if d.TopK != nil {
		if _, exists := chatReq["top_k"]; !exists {
			chatReq["top_k"] = *d.TopK
			applied = append(applied, fmt.Sprintf("top_k=%d", *d.TopK))
		}
	}
	if d.MaxNewTokens != nil {
		if _, exists := chatReq["max_tokens"]; !exists {
			chatReq["max_tokens"] = *d.MaxNewTokens
			applied = append(applied, fmt.Sprintf("max_tokens=%d", *d.MaxNewTokens))
		}
	}
	if d.FrequencyPenalty != nil {
		if _, exists := chatReq["frequency_penalty"]; !exists {
			chatReq["frequency_penalty"] = *d.FrequencyPenalty
			applied = append(applied, fmt.Sprintf("frequency_penalty=%.2f", *d.FrequencyPenalty))
		}
	}
	if d.PresencePenalty != nil {
		if _, exists := chatReq["presence_penalty"]; !exists {
			chatReq["presence_penalty"] = *d.PresencePenalty
			applied = append(applied, fmt.Sprintf("presence_penalty=%.2f", *d.PresencePenalty))
		}
	}
	if d.ReasoningEffort != nil {
		if _, exists := chatReq["reasoning_effort"]; !exists {
			chatReq["reasoning_effort"] = *d.ReasoningEffort
			applied = append(applied, fmt.Sprintf("reasoning_effort=%s", *d.ReasoningEffort))
		}
	}
	if len(d.Stop) > 0 {
		if _, exists := chatReq["stop"]; !exists {
			chatReq["stop"] = d.Stop
			applied = append(applied, fmt.Sprintf("stop=%v", d.Stop))
		}
	}

	if len(applied) > 0 {
		slog.Debug("applied sampling defaults", "model", m.Name, "params", applied)
	}
}
