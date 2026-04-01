package main

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen         string        `yaml:"listen"`
	Models         []ModelConfig `yaml:"models"`
	Keys           []KeyConfig   `yaml:"keys"`
	TrustedProxies []string      `yaml:"trusted_proxies"` // CIDR or IPs allowed to set X-Real-IP
}

const (
	BackendOpenAI    = "openai"
	BackendAnthropic = "anthropic"
)

type ModelConfig struct {
	Name    string `yaml:"name"`
	Backend string `yaml:"backend"`  // upstream URL e.g. http://192.168.100.10:8000/v1
	APIKey  string `yaml:"api_key"`  // key to send to the backend (if required)
	Model   string `yaml:"model"`    // model name to send to the backend (if different from Name)
	Timeout int    `yaml:"timeout"`  // request timeout in seconds (default 300)
	Type    string `yaml:"type"`     // backend type: "" or "openai" (default), "anthropic"
}

type KeyConfig struct {
	Key    string   `yaml:"key"`
	Name   string   `yaml:"name"`   // friendly name for logging
	Models []string `yaml:"models"` // allowed models, empty = all
}

// ConfigStore provides thread-safe access to the current config.
type ConfigStore struct {
	mu     sync.RWMutex
	config *Config
	path   string
}

func NewConfigStore(path string) (*ConfigStore, error) {
	cs := &ConfigStore{path: path}
	if err := cs.Load(); err != nil {
		return nil, err
	}
	return cs, nil
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
		if cfg.Models[i].Timeout == 0 {
			cfg.Models[i].Timeout = 300
		}
		if cfg.Models[i].Model == "" {
			cfg.Models[i].Model = cfg.Models[i].Name
		}
	}

	if err := validateConfig(&cfg); err != nil {
		return err
	}

	cs.mu.Lock()
	cs.config = &cfg
	cs.mu.Unlock()

	slog.Info("config loaded", "models", len(cfg.Models), "keys", len(cfg.Keys))
	return nil
}

func (cs *ConfigStore) Get() *Config {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.config
}

// Watch starts a goroutine that reloads config when the file changes on disk.
// Errors are logged but do not stop the watcher. Returns a stop function.
func (cs *ConfigStore) Watch() (stop func(), err error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("creating file watcher: %w", err)
	}

	if err := watcher.Add(cs.path); err != nil {
		watcher.Close()
		return nil, fmt.Errorf("watching %s: %w", cs.path, err)
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

	slog.Info("watching config file for changes", "path", cs.path)
	return func() { watcher.Close() }, nil
}

func validateConfig(cfg *Config) error {
	if len(cfg.Keys) == 0 {
		slog.Warn("no API keys configured — all requests will be unauthenticated")
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
		default:
			return fmt.Errorf("model %q has unknown type %q (must be %q or %q)", m.Name, m.Type, BackendOpenAI, BackendAnthropic)
		}

		if names[m.Name] {
			return fmt.Errorf("duplicate model name %q", m.Name)
		}
		names[m.Name] = true
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

	return nil
}
