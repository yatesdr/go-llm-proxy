package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

type ProcessorsConfig struct {
	Vision       string `yaml:"vision"`         // legacy single vision model (superseded by vision_models; kept in sync with its head)
	Audio        string `yaml:"audio"`          // model name for audio transcription (empty = disabled; pipeline integration pending)
	OCR          string `yaml:"ocr"`            // model name for OCR/text extraction from PDF page images (falls back to vision)
	WebSearchKey string `yaml:"web_search_key"` // legacy single web search key (superseded by web_search_keys; kept in sync with its head)

	// VisionModels is the priority-ordered vision cascade: the first model is
	// tried first; on failure or empty output the next takes over.
	VisionModels []string `yaml:"vision_models,omitempty"`
	// WebSearchKeys is the priority-ordered search-provider cascade.
	WebSearchKeys []WebSearchKeyEntry `yaml:"web_search_keys,omitempty"`
}

// WebSearchKeyEntry is one provider+key pair in the web-search cascade.
type WebSearchKeyEntry struct {
	Provider string `yaml:"provider" json:"provider"` // "tavily" or "brave"
	Key      string `yaml:"key" json:"key"`
}

// InferSearchProvider guesses the provider from a key's prefix, for legacy
// single-key configs written before providers were explicit.
func InferSearchProvider(key string) string {
	if strings.HasPrefix(key, "BSA") {
		return "brave"
	}
	return "tavily"
}

// EffectiveVisionModels returns the priority-ordered vision cascade, falling
// back to the legacy single vision field for configs that predate lists.
func (p ProcessorsConfig) EffectiveVisionModels() []string {
	if len(p.VisionModels) > 0 {
		return p.VisionModels
	}
	if p.Vision != "" {
		return []string{p.Vision}
	}
	return nil
}

// EffectiveSearchKeys returns the priority-ordered web-search cascade, falling
// back to the legacy single key field for configs that predate lists.
func (p ProcessorsConfig) EffectiveSearchKeys() []WebSearchKeyEntry {
	if len(p.WebSearchKeys) > 0 {
		return p.WebSearchKeys
	}
	if p.WebSearchKey != "" {
		return []WebSearchKeyEntry{{Provider: InferSearchProvider(p.WebSearchKey), Key: p.WebSearchKey}}
	}
	return nil
}

// AudioConfig groups the OpenAI-compatible audio workloads exposed by the
// proxy. Each logical workload owns its backend replicas directly; adding a
// backend automatically adds capacity without creating a separate pool.
type AudioConfig struct {
	Whisper *AudioModelConfig `yaml:"whisper,omitempty"`
	TTS     *AudioModelConfig `yaml:"tts,omitempty"`
}

// AudioModelConfig describes one client-facing audio model and its replicas.
// Whisper is served on the OpenAI transcription/translation routes; TTS is
// served on the OpenAI speech route and the voice discovery extension.
type AudioModelConfig struct {
	Name     string          `yaml:"name"`
	Model    string          `yaml:"model,omitempty"`
	Backends []BackendConfig `yaml:"backends"`
	Timeout  int             `yaml:"timeout,omitempty"`
}

// DocumentsConfig contains document-processing workloads. PaddleOCR is the
// only supported document adapter today, but the capability name remains
// generic so clients do not depend on the implementation.
type DocumentsConfig struct {
	PaddleOCR *PaddleOCRConfig `yaml:"paddleocr,omitempty"`
}

// PaddleOCRConfig configures replicas of the official PaddleOCR layout
// parsing service. Endpoint is the upstream request path; go-llm exposes the
// same contract publicly at /layout-parsing.
type PaddleOCRConfig struct {
	Backends       []BackendConfig `yaml:"backends"`
	Endpoint       string          `yaml:"endpoint,omitempty"`
	HealthEndpoint string          `yaml:"health_endpoint,omitempty"`
	Timeout        int             `yaml:"timeout,omitempty"`
}

type Config struct {
	Listen                 string            `yaml:"listen"`
	Models                 []ModelConfig     `yaml:"models"`
	Aliases                map[string]string `yaml:"aliases,omitempty"` // client-facing alias -> canonical model name
	Keys                   []KeyConfig       `yaml:"keys"`
	Services               ServicesConfig    `yaml:"services"`                 // external service proxies (Qdrant, etc.)
	Processors             ProcessorsConfig  `yaml:"processors"`               // global processor defaults
	Audio                  AudioConfig       `yaml:"audio,omitempty"`          // transcription and text-to-speech workloads
	Documents              DocumentsConfig   `yaml:"documents,omitempty"`      // native document-processing workloads
	TrustedProxies         []string          `yaml:"trusted_proxies"`          // CIDR or IPs allowed to set X-Real-IP
	ServeConfigGenerator   bool              `yaml:"serve_config_generator"`   // enable the config generator page at GET /
	LogMetrics             bool              `yaml:"log_metrics"`              // enable per-request usage logging to SQLite
	UsageDB                string            `yaml:"usage_db"`                 // path to SQLite usage database (default: usage.db)
	UsageDashboard         bool              `yaml:"usage_dashboard"`          // enable the usage dashboard at /usage
	UsageDashboardPassword string            `yaml:"usage_dashboard_password"` // password for the usage dashboard
	AdminPassword          string            `yaml:"admin_password"`           // password for /admin (falls back to usage_dashboard_password)
}

// EffectiveAdminPassword returns the password guarding /admin: admin_password
// when set, otherwise the usage dashboard password (when the dashboard is
// enabled). Empty means the admin UI is disabled.
func (c *Config) EffectiveAdminPassword() string {
	if c.AdminPassword != "" {
		return c.AdminPassword
	}
	if c.UsageDashboard {
		return c.UsageDashboardPassword
	}
	return ""
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
	Name             string            `yaml:"name"`
	Backends         []BackendConfig   `yaml:"backends"`                     // all replicas serving this logical model
	Model            string            `yaml:"model"`                        // model name to send to the backend (if different from Name)
	Timeout          int               `yaml:"timeout"`                      // request timeout in seconds (default 300)
	Type             string            `yaml:"type"`                         // backend type: "" or "openai" (default), "anthropic"
	ResponsesMode    string            `yaml:"responses_mode"`               // "auto" (default), "native", or "translate"
	MessagesMode     string            `yaml:"messages_mode"`                // "auto" (default), "native", or "translate"
	ContextWindow    int               `yaml:"context_window"`               // max context tokens (0 = auto-detect from backend)
	SupportsVision   bool              `yaml:"supports_vision"`              // model handles images natively
	SupportsAudio    bool              `yaml:"supports_audio"`               // model handles audio (transcription or audio input)
	ForcePipeline    bool              `yaml:"force_pipeline,omitempty"`     // deprecated: use RewriteVision/RewriteWebSearch "on" instead; still honored as legacy "force both on"
	RewriteVision    string            `yaml:"rewrite_vision,omitempty"`     // "" (auto by protocol), "on", or "off" — image_url content only
	RewriteDocuments string            `yaml:"rewrite_documents,omitempty"`  // "" (auto by protocol), "on", or "off" — pdf_data content only
	RewriteWebSearch string            `yaml:"rewrite_web_search,omitempty"` // "" (auto by protocol), "on", or "off"
	Processors       *ProcessorsConfig `yaml:"processors"`                   // per-model processor overrides (nil = use global)
	Defaults         *SamplingDefaults `yaml:"defaults"`                     // default sampling parameters (nil = use backend defaults)

	// AWS Bedrock fields (only used when type: "bedrock").
	// If api_key is set, it is sent as a Bedrock API key bearer token and the
	// SigV4 fields below are ignored. Otherwise SigV4 signing is used with the
	// provided IAM credentials, falling back to AWS_ACCESS_KEY_ID /
	// AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN environment variables.
	Region          string `yaml:"region"`            // AWS region, e.g. "us-east-1"
	AWSAccessKey    string `yaml:"aws_access_key"`    // IAM access key ID (AKIA...)
	AWSSecretKey    string `yaml:"aws_secret_key"`    // IAM secret access key
	AWSSessionToken string `yaml:"aws_session_token"` // optional STS session token

	// Optional Bedrock guardrail configuration. Applied to every request to
	// this model; per-request override is not supported by design. Trace
	// accepts "enabled", "disabled", or "enabled_full" per Bedrock API.
	GuardrailID      string `yaml:"guardrail_id,omitempty"`
	GuardrailVersion string `yaml:"guardrail_version,omitempty"`
	GuardrailTrace   string `yaml:"guardrail_trace,omitempty"`

	// Backend and APIKey are request-scoped compatibility fields populated by
	// finalizeConfig and the load balancer. They are never serialized: every
	// configured model owns a backends list, even when it contains one server.
	Backend string `yaml:"-"`
	APIKey  string `yaml:"-"`
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
	writeMu  sync.Mutex // serializes in-process writes to the config file
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

	data, migrated, err := migrateLegacyBackendConfig(data)
	if err != nil {
		return fmt.Errorf("migrating legacy backend config: %w", err)
	}
	if migrated {
		if err := persistMigratedConfig(cs.path, data); err != nil {
			return fmt.Errorf("saving migrated backend config: %w", err)
		}
		slog.Info("migrated config to model-owned backend lists", "backup", cs.path+legacyConfigBackupSuffix)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}

	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}

	if err := finalizeConfig(&cfg); err != nil {
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

// applyPasswordEnvDefaults fills admin_password / usage_dashboard_password
// from the environment when the config file leaves them unset — lets a
// first-time Docker deployment bootstrap credentials via a .env file instead
// of hand-editing YAML. The YAML value always wins when set; the env var is
// only a fallback for an empty field, same precedence as the Bedrock AWS
// credential fallback below.
func applyPasswordEnvDefaults(cfg *Config) {
	if cfg.AdminPassword == "" {
		cfg.AdminPassword = os.Getenv("GO_LLM_ADMIN_PASSWORD")
	}
	if cfg.UsageDashboardPassword == "" {
		cfg.UsageDashboardPassword = os.Getenv("GO_LLM_USAGE_DASHBOARD_PASSWORD")
	}
}

// finalizeConfig applies defaults, validates the whole config, and bridges
// each model's first configured replica into request-scoped compatibility
// fields used by protocol adapters before the load balancer selects a replica.
// Shared by Load() and the admin save path so both enforce identical rules.
func finalizeConfig(cfg *Config) error {
	applyPasswordEnvDefaults(cfg)

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
	for _, audio := range []*AudioModelConfig{cfg.Audio.Whisper, cfg.Audio.TTS} {
		if audio == nil {
			continue
		}
		if audio.Model == "" {
			audio.Model = audio.Name
		}
		if audio.Timeout == 0 {
			audio.Timeout = 300
		}
	}
	if doc := cfg.Documents.PaddleOCR; doc != nil {
		if doc.Endpoint == "" {
			doc.Endpoint = "/layout-parsing"
		}
		if doc.HealthEndpoint == "" {
			doc.HealthEndpoint = "/health"
		}
		if doc.Timeout == 0 {
			doc.Timeout = 300
		}
	}

	if err := validateConfig(cfg); err != nil {
		return err
	}

	// Bridge code that predates replica lists. The load balancer overwrites
	// these request-scoped fields with the actual selected backend.
	for i := range cfg.Models {
		m := &cfg.Models[i]
		backends := cfg.EffectiveBackends(m)
		if len(backends) > 0 {
			m.Backend = backends[0].URL
			if m.APIKey == "" {
				m.APIKey = backends[0].APIKey
			}
		}
	}
	return nil
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

// ResolveModelName returns the canonical configured model name for name.
// Real model names take precedence over aliases; unknown names are returned
// unchanged so callers can preserve the requested ID in errors and capture.
func ResolveModelName(cfg *Config, name string) string {
	if cfg == nil {
		return name
	}
	if FindModel(cfg, name) != nil {
		return name
	}
	if target, ok := cfg.Aliases[name]; ok {
		return target
	}
	return name
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
	for i := range cfg.Models {
		m := &cfg.Models[i]
		if m.Name == "" {
			return fmt.Errorf("model entry missing name")
		}

		if len(m.Backends) == 0 {
			if m.Backend == "" {
				return fmt.Errorf("model %q missing backend list", m.Name)
			}
			m.Backends = []BackendConfig{{URL: m.Backend, APIKey: m.APIKey, Weight: 1}}
		}
		if err := validateBackendList(fmt.Sprintf("model %q", m.Name), m.Backends); err != nil {
			return err
		}

		switch m.Type {
		case "", BackendOpenAI, BackendAnthropic:
		case BackendBedrock:
			if m.Region == "" {
				return fmt.Errorf("model %q (bedrock) requires region", m.Name)
			}
			hasAPIKey := false
			for _, b := range m.Backends {
				if b.APIKey != "" {
					hasAPIKey = true
					break
				}
			}
			if !hasAPIKey && m.APIKey == "" && (m.AWSAccessKey == "" || m.AWSSecretKey == "") {
				return fmt.Errorf("model %q (bedrock) requires either api_key (Bedrock API key) or aws_access_key + aws_secret_key (set in config or environment)", m.Name)
			}
			// Soft check: cross-region inference profile IDs are prefixed with
			// a region scope (us., eu., apac., us-gov.). When the prefix and
			// the configured region obviously disagree, warn at startup —
			// the request would otherwise fail opaquely at the first call.
			// Not a hard error: AWS periodically adds new scope prefixes and
			// we don't want to gate startup on our allowlist keeping up.
			warnIfCrossRegionProfileMismatch(m.Name, m.Model, m.Region)
			if m.MessagesMode != "" && m.MessagesMode != "auto" {
				slog.Warn("messages_mode has no effect on bedrock backends (always translates to Converse)",
					"model", m.Name, "messages_mode", m.MessagesMode)
			}
			if m.ResponsesMode != "" && m.ResponsesMode != "auto" {
				slog.Warn("responses_mode has no effect on bedrock backends (Responses API is not supported)",
					"model", m.Name, "responses_mode", m.ResponsesMode)
			}
		default:
			return fmt.Errorf("model %q has unknown type %q (must be %q, %q, or %q)", m.Name, m.Type, BackendOpenAI, BackendAnthropic, BackendBedrock)
		}

		switch m.RewriteVision {
		case "", "on", "off":
		default:
			return fmt.Errorf("model %q has unknown rewrite_vision %q (must be \"on\", \"off\", or omitted)", m.Name, m.RewriteVision)
		}
		switch m.RewriteDocuments {
		case "", "on", "off":
		default:
			return fmt.Errorf("model %q has unknown rewrite_documents %q (must be \"on\", \"off\", or omitted)", m.Name, m.RewriteDocuments)
		}
		switch m.RewriteWebSearch {
		case "", "on", "off":
		default:
			return fmt.Errorf("model %q has unknown rewrite_web_search %q (must be \"on\", \"off\", or omitted)", m.Name, m.RewriteWebSearch)
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

	// Aliases are deliberately single-level: every target must be a concrete
	// chat model, and alias IDs may not shadow configured model names.
	for alias, target := range cfg.Aliases {
		if alias == "" {
			return fmt.Errorf("alias name is required")
		}
		if names[alias] {
			return fmt.Errorf("alias %q conflicts with model name", alias)
		}
		if _, isAlias := cfg.Aliases[target]; isAlias {
			return fmt.Errorf("alias %q may not target alias %q", alias, target)
		}
		if !names[target] {
			return fmt.Errorf("alias %q references unknown model %q", alias, target)
		}
	}

	for label, audio := range map[string]*AudioModelConfig{
		"audio.whisper": cfg.Audio.Whisper,
		"audio.tts":     cfg.Audio.TTS,
	} {
		if audio == nil {
			continue
		}
		if audio.Name == "" {
			return fmt.Errorf("%s missing name", label)
		}
		if names[audio.Name] {
			return fmt.Errorf("%s name %q conflicts with a chat model", label, audio.Name)
		}
		if len(audio.Backends) == 0 {
			return fmt.Errorf("%s has no backends", label)
		}
		if err := validateBackendList(label, audio.Backends); err != nil {
			return err
		}
		if audio.Timeout < 1 {
			return fmt.Errorf("%s timeout must be positive", label)
		}
		names[audio.Name] = true
	}

	if doc := cfg.Documents.PaddleOCR; doc != nil {
		if len(doc.Backends) == 0 {
			return fmt.Errorf("documents.paddleocr has no backends")
		}
		if err := validateBackendList("documents.paddleocr", doc.Backends); err != nil {
			return err
		}
		if doc.Endpoint == "" || doc.Endpoint[0] != '/' {
			return fmt.Errorf("documents.paddleocr endpoint must be an absolute path")
		}
		if doc.HealthEndpoint == "" || doc.HealthEndpoint[0] != '/' {
			return fmt.Errorf("documents.paddleocr health_endpoint must be an absolute path")
		}
		if doc.Timeout < 1 {
			return fmt.Errorf("documents.paddleocr timeout must be positive")
		}
	}

	// Validate global vision processor references a defined model.
	if v := cfg.Processors.Vision; v != "" {
		if !names[v] {
			return fmt.Errorf("global processors.vision references unknown model %q", v)
		}
	}
	for _, v := range cfg.Processors.VisionModels {
		if !names[v] {
			return fmt.Errorf("processors.vision_models references unknown model %q", v)
		}
	}
	for i, e := range cfg.Processors.WebSearchKeys {
		if e.Provider != "tavily" && e.Provider != "brave" {
			return fmt.Errorf("processors.web_search_keys[%d]: provider must be tavily or brave", i)
		}
		if e.Key == "" {
			return fmt.Errorf("processors.web_search_keys[%d]: key is empty", i)
		}
	}

	// Validate global OCR processor references a defined model.
	if v := cfg.Processors.OCR; v != "" && v != "none" {
		if !names[v] {
			return fmt.Errorf("global processors.ocr references unknown model %q", v)
		}
	}

	// Validate global audio processor references a defined model.
	if v := cfg.Processors.Audio; v != "" && v != "none" {
		if !names[v] {
			return fmt.Errorf("global processors.audio references unknown model %q", v)
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
	for _, v := range cfg.Processors.VisionModels {
		visionModels[v] = true
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

	// Auto-infer SupportsAudio: any model referenced as the global audio
	// processor obviously handles audio — don't require it to be set twice.
	if cfg.Processors.Audio != "" && cfg.Processors.Audio != "none" {
		for i := range cfg.Models {
			if cfg.Models[i].Name == cfg.Processors.Audio && !cfg.Models[i].SupportsAudio {
				cfg.Models[i].SupportsAudio = true
			}
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

// warnIfCrossRegionProfileMismatch emits a startup warning when a Bedrock
// cross-region inference profile ID carries a region-scope prefix that
// obviously disagrees with the configured region.
//
// Example: model "eu.anthropic.claude-3-5-sonnet-20241022-v2:0" with region
// "us-east-1" will fail at first request because the EU profile is only
// served from EU regions. Catching this at startup turns an opaque 400 into
// an actionable warning.
//
// Deliberately NOT a hard error: AWS adds new scope prefixes periodically
// (apac., us-gov., future mc-...) and a strict allowlist would make
// upgrades painful.
func warnIfCrossRegionProfileMismatch(modelName, modelID, region string) {
	// Known region-scope prefixes as of 2026-04. Extend as new ones appear.
	scopes := map[string]string{
		"us.":     "us-",
		"eu.":     "eu-",
		"apac.":   "ap-",
		"us-gov.": "us-gov-",
	}
	for prefix, regionStub := range scopes {
		if !stringHasPrefix(modelID, prefix) {
			continue
		}
		if !stringHasPrefix(region, regionStub) {
			slog.Warn("bedrock inference profile region prefix may not match configured region",
				"model", modelName, "model_id", modelID, "region", region,
				"expected_region_prefix", regionStub)
		}
		return
	}
}

// stringHasPrefix is a tiny inlineable helper — pulled out so the warning
// function above stays readable without pulling in strings just for HasPrefix.
func stringHasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
