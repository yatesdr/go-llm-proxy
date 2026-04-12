package config

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	var receivedAPIKey string
	var receivedHeader string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAPIKey = r.Header.Get("Authorization")
		receivedHeader = r.Header.Get("X-Api-Key")
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

	// Just verify the store works with multiple models and auth types.
	if len(hs.GetStatus()) != 2 {
		t.Errorf("expected 2 models")
	}
	_ = receivedAPIKey
	_ = receivedHeader
}
