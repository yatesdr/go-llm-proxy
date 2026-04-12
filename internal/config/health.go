package config

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// ModelHealth represents the health status of a single model backend.
type ModelHealth struct {
	Name      string    `json:"name"`
	Online    bool      `json:"online"`
	LastCheck time.Time `json:"last_check"`
	Error     string    `json:"error,omitempty"`
	External  bool      `json:"external"` // true if backend is an external API (not polled periodically)
}

// HealthStore manages health status for all configured models.
// All methods are safe for concurrent use.
type HealthStore struct {
	config        *ConfigStore
	mu            sync.RWMutex
	health        map[string]*ModelHealth
	stopCh        chan struct{}
	wg            sync.WaitGroup
	checkInterval time.Duration
	checkTimeout  time.Duration
}

// NewHealthStore initializes a health store from the current config.
func NewHealthStore(cs *ConfigStore, interval, timeout time.Duration) *HealthStore {
	hs := &HealthStore{
		config:        cs,
		health:        make(map[string]*ModelHealth),
		stopCh:        make(chan struct{}),
		checkInterval: interval,
		checkTimeout:  timeout,
	}
	hs.initFromConfig()
	return hs
}

func (hs *HealthStore) initFromConfig() {
	cfg := hs.config.Get()
	hs.mu.Lock()
	defer hs.mu.Unlock()
	for _, m := range cfg.Models {
		hs.health[m.Name] = &ModelHealth{
			Name:     m.Name,
			Online:   true,
			External: isExternalBackend(m.Backend),
		}
	}
}

// isExternalBackend returns true if the backend URL points to an external API
// (not localhost or a private IP range). External backends are checked once
// at startup and then updated based on actual usage, not periodic polling.
func isExternalBackend(backendURL string) bool {
	u, err := url.Parse(backendURL)
	if err != nil {
		return false // can't parse, assume local
	}

	host := u.Hostname()

	// Localhost variants
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return false
	}

	// Try to parse as IP
	ip := net.ParseIP(host)
	if ip != nil {
		// Check private IP ranges (RFC 1918 + link-local)
		privateRanges := []string{
			"10.0.0.0/8",
			"172.16.0.0/12",
			"192.168.0.0/16",
			"169.254.0.0/16", // link-local
			"fc00::/7",       // IPv6 unique local
			"fe80::/10",      // IPv6 link-local
		}
		for _, cidr := range privateRanges {
			_, network, err := net.ParseCIDR(cidr)
			if err == nil && network.Contains(ip) {
				return false
			}
		}
		return true // public IP
	}

	// Not an IP, must be a hostname - if it's not localhost, assume external
	return true
}

// RefreshFromConfig syncs the health map after a config reload.
func (hs *HealthStore) RefreshFromConfig() {
	cfg := hs.config.Get()
	hs.mu.Lock()
	defer hs.mu.Unlock()

	configModels := make(map[string]bool, len(cfg.Models))
	for _, m := range cfg.Models {
		configModels[m.Name] = true
	}

	for _, m := range cfg.Models {
		if _, exists := hs.health[m.Name]; !exists {
			hs.health[m.Name] = &ModelHealth{
				Name:     m.Name,
				Online:   true,
				External: isExternalBackend(m.Backend),
			}
		}
	}

	for name := range hs.health {
		if !configModels[name] {
			delete(hs.health, name)
		}
	}
}

// GetStatus returns a copy of all model health statuses.
func (hs *HealthStore) GetStatus() map[string]ModelHealth {
	hs.mu.RLock()
	defer hs.mu.RUnlock()

	result := make(map[string]ModelHealth, len(hs.health))
	for name, h := range hs.health {
		result[name] = ModelHealth{
			Name:      h.Name,
			Online:    h.Online,
			LastCheck: h.LastCheck,
			Error:     h.Error,
			External:  h.External,
		}
	}
	return result
}

// GetStatusForModel returns the health status for a specific model.
func (hs *HealthStore) GetStatusForModel(name string) (ModelHealth, bool) {
	hs.mu.RLock()
	defer hs.mu.RUnlock()

	h, ok := hs.health[name]
	if !ok {
		return ModelHealth{}, false
	}
	return ModelHealth{
		Name:      h.Name,
		Online:    h.Online,
		LastCheck: h.LastCheck,
		Error:     h.Error,
		External:  h.External,
	}, true
}

// Start begins periodic health checking of all model backends.
func (hs *HealthStore) Start(ctx context.Context) {
	hs.mu.Lock()
	select {
	case <-hs.stopCh:
		hs.mu.Unlock()
		return
	default:
	}
	hs.mu.Unlock()

	slog.Info("health checker starting",
		"interval", hs.checkInterval,
		"timeout", hs.checkTimeout)

	client := &http.Client{
		Timeout: hs.checkTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	hs.wg.Add(1)
	go hs.runChecker(ctx, client)
}

// Stop gracefully stops the health checker.
func (hs *HealthStore) Stop() {
	hs.mu.Lock()
	select {
	case <-hs.stopCh:
		hs.mu.Unlock()
		return
	default:
		close(hs.stopCh)
	}
	hs.mu.Unlock()

	hs.wg.Wait()
	slog.Info("health checker stopped")
}

func (hs *HealthStore) runChecker(ctx context.Context, client *http.Client) {
	defer hs.wg.Done()

	ticker := time.NewTicker(hs.checkInterval)
	defer ticker.Stop()

	// Initial check includes ALL backends (including external).
	hs.checkAllInitial(ctx, client)

	for {
		select {
		case <-ctx.Done():
			return
		case <-hs.stopCh:
			return
		case <-ticker.C:
			// Periodic checks skip external backends.
			hs.checkAll(ctx, client)
		}
	}
}

// checkAll checks all local (non-external) model backends.
// External backends are only checked once at startup via checkAllInitial.
func (hs *HealthStore) checkAll(ctx context.Context, client *http.Client) {
	cfg := hs.config.Get()
	for i := range cfg.Models {
		m := &cfg.Models[i]
		// Skip external backends - they're updated via RecordUsage instead.
		if isExternalBackend(m.Backend) {
			continue
		}
		hs.wg.Add(1)
		go hs.checkOne(ctx, client, m)
	}
}

// checkAllInitial checks ALL backends including external ones.
// Called once at startup to establish initial state.
func (hs *HealthStore) checkAllInitial(ctx context.Context, client *http.Client) {
	cfg := hs.config.Get()
	for i := range cfg.Models {
		m := &cfg.Models[i]
		hs.wg.Add(1)
		go hs.checkOne(ctx, client, m)
	}
}

func (hs *HealthStore) checkOne(ctx context.Context, client *http.Client, m *ModelConfig) {
	defer hs.wg.Done()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, m.Backend, nil)
	if err != nil {
		hs.updateHealth(m.Name, false, "invalid backend URL: "+err.Error())
		return
	}

	if m.APIKey != "" {
		if m.Type == BackendAnthropic {
			req.Header.Set("X-Api-Key", m.APIKey)
		} else {
			req.Header.Set("Authorization", "Bearer "+m.APIKey)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		hs.updateHealth(m.Name, false, err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		hs.updateHealth(m.Name, false, fmt.Sprintf("server error: HTTP %d", resp.StatusCode))
		return
	}

	if resp.StatusCode < 500 {
		hs.updateHealth(m.Name, true, "")
		return
	}
}

func (hs *HealthStore) updateHealth(name string, online bool, errMsg string) {
	hs.mu.Lock()
	defer hs.mu.Unlock()

	h, ok := hs.health[name]
	if !ok {
		return
	}

	h.Online = online
	h.LastCheck = time.Now()
	h.Error = errMsg

	if online {
		slog.Debug("health: model online", "model", name)
	} else {
		slog.Info("health: model offline", "model", name, "error", errMsg)
	}
}

// RecordUsage updates health status based on actual request results.
// This is the primary health tracking mechanism for external backends,
// which are not polled periodically to avoid spamming external APIs.
// Call this after each request completes (success or failure).
func (hs *HealthStore) RecordUsage(name string, success bool, errMsg string) {
	hs.mu.Lock()
	defer hs.mu.Unlock()

	h, ok := hs.health[name]
	if !ok {
		return
	}

	// Only log state changes to reduce noise.
	wasOnline := h.Online

	h.Online = success
	h.LastCheck = time.Now()
	if success {
		h.Error = ""
	} else {
		h.Error = errMsg
	}

	// Log state transitions for external backends.
	if h.External && wasOnline != success {
		if success {
			slog.Info("health: external model back online", "model", name)
		} else {
			slog.Info("health: external model offline", "model", name, "error", errMsg)
		}
	}
}
