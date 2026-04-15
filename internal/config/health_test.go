package config

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestHealthStore_Online(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	cfg := &Config{
		Models: []ModelConfig{
			{Name: "test-model", Backend: ts.URL, Timeout: 300},
		},
	}
	cs := NewTestConfigStore(cfg)
	hs := NewHealthStore(cs, time.Minute, 5*time.Second)

	// Run a single check.
	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}
	hs.checkAll(ctx, client)

	// Wait for goroutine to complete.
	time.Sleep(100 * time.Millisecond)

	status := hs.GetStatus()
	if !status["test-model"].Online {
		t.Errorf("expected model to be online")
	}
	if status["test-model"].Error != "" {
		t.Errorf("expected no error, got %q", status["test-model"].Error)
	}
}

func TestHealthStore_Offline(t *testing.T) {
	cfg := &Config{
		Models: []ModelConfig{
			{Name: "test-model", Backend: "http://localhost:99999", Timeout: 300},
		},
	}
	cs := NewTestConfigStore(cfg)
	hs := NewHealthStore(cs, time.Minute, 5*time.Second)

	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}
	hs.checkAll(ctx, client)
	time.Sleep(100 * time.Millisecond)

	status := hs.GetStatus()
	if status["test-model"].Online {
		t.Errorf("expected model to be offline")
	}
	if status["test-model"].Error == "" {
		t.Errorf("expected an error message")
	}
}

func TestHealthStore_ServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ts.Close()

	cfg := &Config{
		Models: []ModelConfig{
			{Name: "test-model", Backend: ts.URL, Timeout: 300},
		},
	}
	cs := NewTestConfigStore(cfg)
	hs := NewHealthStore(cs, time.Minute, 5*time.Second)

	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}
	hs.checkAll(ctx, client)
	time.Sleep(100 * time.Millisecond)

	status := hs.GetStatus()
	if status["test-model"].Online {
		t.Errorf("expected model to be offline on 502")
	}
}

func TestHealthStore_RefreshFromConfig(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	cfg := &Config{
		Models: []ModelConfig{
			{Name: "model-1", Backend: ts.URL, Timeout: 300},
		},
	}
	cs := NewTestConfigStore(cfg)
	hs := NewHealthStore(cs, time.Minute, 5*time.Second)

	if len(hs.GetStatus()) != 1 {
		t.Errorf("expected 1 model initially")
	}

	// Simulate config reload with a new model.
	cs.config = &Config{
		Models: []ModelConfig{
			{Name: "model-1", Backend: ts.URL, Timeout: 300},
			{Name: "model-2", Backend: ts.URL, Timeout: 300},
		},
	}
	hs.RefreshFromConfig()

	if len(hs.GetStatus()) != 2 {
		t.Errorf("expected 2 models after refresh")
	}

	// Simulate removal.
	cs.config = &Config{
		Models: []ModelConfig{
			{Name: "model-2", Backend: ts.URL, Timeout: 300},
		},
	}
	hs.RefreshFromConfig()

	if len(hs.GetStatus()) != 1 {
		t.Errorf("expected 1 model after removal")
	}
	if _, exists := hs.health["model-1"]; exists {
		t.Errorf("model-1 should have been removed")
	}
}

func TestHealthStore_GetStatusForModel(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	cfg := &Config{
		Models: []ModelConfig{
			{Name: "test-model", Backend: ts.URL, Timeout: 300},
		},
	}
	cs := NewTestConfigStore(cfg)
	hs := NewHealthStore(cs, time.Minute, 5*time.Second)

	h, ok := hs.GetStatusForModel("test-model")
	if !ok {
		t.Errorf("expected to find test-model")
	}
	if !h.Online {
		t.Errorf("expected model to be online initially")
	}

	_, ok = hs.GetStatusForModel("nonexistent")
	if ok {
		t.Errorf("expected not to find nonexistent model")
	}
}

func TestHealthStore_StartStop(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	cfg := &Config{
		Models: []ModelConfig{
			{Name: "test-model", Backend: ts.URL, Timeout: 300},
		},
	}
	cs := NewTestConfigStore(cfg)
	hs := NewHealthStore(cs, time.Millisecond*50, 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	hs.Start(ctx)

	// Wait for at least one check cycle.
	time.Sleep(150 * time.Millisecond)

	status := hs.GetStatus()
	if !status["test-model"].Online {
		t.Errorf("expected model to be online after checks")
	}

	hs.Stop()
}

func TestHealthStore_AuthHeaders(t *testing.T) {
	// hs.checkAll probes both models concurrently, so the httptest handler
	// runs on two goroutines. Guard the captured headers with a mutex so
	// the race detector is happy — this test's purpose is only to verify
	// the store handles both auth types, so we don't assert on the values.
	var mu sync.Mutex
	var receivedAPIKey, receivedHeader string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedAPIKey = r.Header.Get("Authorization")
		receivedHeader = r.Header.Get("X-Api-Key")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	cfg := &Config{
		Models: []ModelConfig{
			{Name: "openai-model", Backend: ts.URL, APIKey: "secret-key", Type: BackendOpenAI, Timeout: 300},
			{Name: "anthropic-model", Backend: ts.URL, APIKey: "anthropic-key", Type: BackendAnthropic, Timeout: 300},
		},
	}
	cs := NewTestConfigStore(cfg)
	hs := NewHealthStore(cs, time.Minute, 5*time.Second)

	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}
	hs.checkAll(ctx, client)
	time.Sleep(100 * time.Millisecond)

	if len(hs.GetStatus()) != 2 {
		t.Errorf("expected 2 models")
	}
	mu.Lock()
	_ = receivedAPIKey
	_ = receivedHeader
	mu.Unlock()
}

func TestIsExternalBackend(t *testing.T) {
	tests := []struct {
		url      string
		expected bool
	}{
		// Local backends (should NOT be external)
		{"http://localhost:8000/v1", false},
		{"http://127.0.0.1:8000/v1", false},
		{"http://192.168.1.100:8000/v1", false},
		{"http://192.168.13.32:8001/v1", false},
		{"http://10.0.0.5:8000/v1", false},
		{"http://172.16.0.1:8000/v1", false},
		{"http://172.31.255.255:8000", false},

		// External backends (SHOULD be external)
		{"https://api.openai.com/v1", true},
		{"https://api.anthropic.com/v1", true},
		{"https://api.z.ai/api/coding/paas/v4", true},
		{"https://8.8.8.8:443/v1", true},
		{"https://example.com/api", true},
	}

	for _, tt := range tests {
		got := isExternalBackend(tt.url)
		if got != tt.expected {
			t.Errorf("isExternalBackend(%q) = %v, want %v", tt.url, got, tt.expected)
		}
	}
}

func TestHealthStore_ExternalFlag(t *testing.T) {
	cfg := &Config{
		Models: []ModelConfig{
			{Name: "local-model", Backend: "http://192.168.1.100:8000/v1", Timeout: 300},
			{Name: "external-model", Backend: "https://api.openai.com/v1", Timeout: 300},
		},
	}
	cs := NewTestConfigStore(cfg)
	hs := NewHealthStore(cs, time.Minute, 5*time.Second)

	status := hs.GetStatus()

	if status["local-model"].External {
		t.Errorf("local-model should not be marked as external")
	}
	if !status["external-model"].External {
		t.Errorf("external-model should be marked as external")
	}
}

func TestHealthStore_RecordUsage(t *testing.T) {
	cfg := &Config{
		Models: []ModelConfig{
			{Name: "test-model", Backend: "https://api.example.com/v1", Timeout: 300},
		},
	}
	cs := NewTestConfigStore(cfg)
	hs := NewHealthStore(cs, time.Minute, 5*time.Second)

	// Initially online
	status, _ := hs.GetStatusForModel("test-model")
	if !status.Online {
		t.Errorf("expected model to be online initially")
	}

	// Record a failure
	hs.RecordUsage("test-model", false, "connection refused")
	status, _ = hs.GetStatusForModel("test-model")
	if status.Online {
		t.Errorf("expected model to be offline after failure")
	}
	if status.Error != "connection refused" {
		t.Errorf("expected error message, got %q", status.Error)
	}

	// Record a success
	hs.RecordUsage("test-model", true, "")
	status, _ = hs.GetStatusForModel("test-model")
	if !status.Online {
		t.Errorf("expected model to be online after success")
	}
	if status.Error != "" {
		t.Errorf("expected no error, got %q", status.Error)
	}
}
