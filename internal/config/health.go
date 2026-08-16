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

// BackendHealth represents the probed health of a single backend URL.
// Backends are probed individually so one dead replica does not hide or block
// the other replicas serving the same workload.
type BackendHealth struct {
	URL       string    `json:"url"`
	Online    bool      `json:"online"`
	LastCheck time.Time `json:"last_check"`
	Error     string    `json:"error,omitempty"`
	External  bool      `json:"external"`
}

// HealthStore manages health status for all configured models.
// All methods are safe for concurrent use.
type HealthStore struct {
	config        *ConfigStore
	mu            sync.RWMutex
	health        map[string]*ModelHealth
	backends      map[string]*BackendHealth // keyed by backend URL
	onBackend     func(url string, online bool)
	stopCh        chan struct{}
	wg            sync.WaitGroup
	checkInterval time.Duration
	checkTimeout  time.Duration
}

// SetBackendListener registers a callback invoked after every backend probe
// with the probed URL and result. Used to feed the load balancer's health
// gate. Call once during startup, before Start.
func (hs *HealthStore) SetBackendListener(fn func(url string, online bool)) {
	hs.onBackend = fn
}

// backendRef aggregates everything needed to probe one unique backend URL:
// credentials, protocol type, and the models it serves.
type backendRef struct {
	url      string
	apiKey   string
	typ      string
	external bool
	models   []string
}

// collectBackendRefs builds the set of unique backend URLs across chat, audio,
// and document workloads, remembering which chat models each URL serves.
func collectBackendRefs(cfg *Config) map[string]*backendRef {
	refs := make(map[string]*backendRef)
	for i := range cfg.Models {
		m := &cfg.Models[i]
		for _, b := range cfg.EffectiveBackends(m) {
			ref, ok := refs[b.URL]
			if !ok {
				ref = &backendRef{
					url:      b.URL,
					apiKey:   b.APIKey,
					typ:      m.Type,
					external: isExternalBackend(b.URL),
				}
				refs[b.URL] = ref
			}
			ref.models = append(ref.models, m.Name)
		}
	}
	addWorkload := func(backends []BackendConfig) {
		for _, b := range backends {
			if _, ok := refs[b.URL]; !ok {
				refs[b.URL] = &backendRef{url: b.URL, apiKey: b.APIKey, external: isExternalBackend(b.URL)}
			}
		}
	}
	if cfg.Audio.Whisper != nil {
		addWorkload(cfg.Audio.Whisper.Backends)
	}
	if cfg.Audio.TTS != nil {
		addWorkload(cfg.Audio.TTS.Backends)
	}
	if cfg.Documents.PaddleOCR != nil {
		addWorkload(cfg.Documents.PaddleOCR.Backends)
	}
	return refs
}

// NewHealthStore initializes a health store from the current config.
func NewHealthStore(cs *ConfigStore, interval, timeout time.Duration) *HealthStore {
	hs := &HealthStore{
		config:        cs,
		health:        make(map[string]*ModelHealth),
		backends:      make(map[string]*BackendHealth),
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

// RecheckAll triggers a one-shot health probe of ALL backends, including
// external ones. Call it after a config reload so that changed backends/keys
// are re-evaluated immediately instead of showing a status frozen from the
// last process start. Probes run asynchronously; the call does not block.
func (hs *HealthStore) RecheckAll() {
	client := &http.Client{
		Timeout: hs.checkTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	hs.checkAllInitial(context.Background(), client)
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

	// Prune backend entries whose URL no longer appears in any workload.
	live := collectBackendRefs(cfg)
	for url := range hs.backends {
		if _, ok := live[url]; !ok {
			delete(hs.backends, url)
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

// checkAll probes all local (non-external) backend URLs.
// External backends are only checked once at startup via checkAllInitial.
func (hs *HealthStore) checkAll(ctx context.Context, client *http.Client) {
	for _, ref := range collectBackendRefs(hs.config.Get()) {
		// Skip external backends - they're updated via RecordUsage instead.
		if ref.external {
			continue
		}
		hs.wg.Add(1)
		go hs.checkOne(ctx, client, ref)
	}
}

// checkAllInitial probes ALL backend URLs including external ones.
// Called once at startup to establish initial state.
func (hs *HealthStore) checkAllInitial(ctx context.Context, client *http.Client) {
	for _, ref := range collectBackendRefs(hs.config.Get()) {
		hs.wg.Add(1)
		go hs.checkOne(ctx, client, ref)
	}
}

func (hs *HealthStore) checkOne(ctx context.Context, client *http.Client, ref *backendRef) {
	defer hs.wg.Done()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, ref.url, nil)
	if err != nil {
		hs.updateBackendHealth(ref, false, "invalid backend URL: "+err.Error())
		return
	}

	if ref.apiKey != "" {
		if ref.typ == BackendAnthropic {
			req.Header.Set("X-Api-Key", ref.apiKey)
		} else {
			req.Header.Set("Authorization", "Bearer "+ref.apiKey)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		hs.updateBackendHealth(ref, false, err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		hs.updateBackendHealth(ref, false, fmt.Sprintf("server error: HTTP %d", resp.StatusCode))
		return
	}
	hs.updateBackendHealth(ref, true, "")
}

// updateBackendHealth records a probe result for one backend URL, feeds the
// load balancer's health gate, and recomputes health for every model the
// backend serves (a model is online while ANY of its replicas is).
func (hs *HealthStore) updateBackendHealth(ref *backendRef, online bool, errMsg string) {
	cfg := hs.config.Get()
	now := time.Now()

	hs.mu.Lock()
	bh, ok := hs.backends[ref.url]
	if !ok {
		bh = &BackendHealth{URL: ref.url, External: ref.external}
		hs.backends[ref.url] = bh
	}
	prevOnline := bh.Online || bh.LastCheck.IsZero()
	bh.Online = online
	bh.LastCheck = now
	bh.Error = errMsg

	// Recompute each served model's health from all of its backends.
	for _, name := range ref.models {
		h, ok := hs.health[name]
		if !ok {
			continue
		}
		m := FindModel(cfg, name)
		if m == nil {
			continue
		}
		anyOnline := false
		firstErr := ""
		for _, b := range cfg.EffectiveBackends(m) {
			if b.Disabled {
				continue
			}
			known, seen := hs.backends[b.URL]
			if !seen || known.Online {
				anyOnline = true
				break
			}
			if firstErr == "" {
				firstErr = known.Error
			}
		}
		h.Online = anyOnline
		h.LastCheck = now
		if anyOnline {
			h.Error = ""
		} else {
			h.Error = firstErr
		}
	}
	hs.mu.Unlock()

	if online != prevOnline {
		if online {
			slog.Info("health: backend online", "backend", ref.url)
		} else {
			slog.Info("health: backend offline", "backend", ref.url, "error", errMsg)
		}
	}

	if hs.onBackend != nil {
		hs.onBackend(ref.url, online)
	}
}

// GetBackendStatus returns a copy of all probed backend health entries,
// keyed by backend URL.
func (hs *HealthStore) GetBackendStatus() map[string]BackendHealth {
	hs.mu.RLock()
	defer hs.mu.RUnlock()
	out := make(map[string]BackendHealth, len(hs.backends))
	for url, b := range hs.backends {
		out[url] = *b
	}
	return out
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
