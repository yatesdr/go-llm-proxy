package handler

import (
	"strings"
	"testing"
	"time"

	"go-llm-proxy/internal/config"
)

// TestRecordBackendHealth verifies that only backend-caused status codes move a
// model's health: 2xx → online, 401/402/403 and 5xx → offline, while client
// errors (4xx) and rate-limits (429) leave the existing status untouched.
func TestRecordBackendHealth(t *testing.T) {
	cfg := &config.Config{
		Models: []config.ModelConfig{
			{Name: "m", Backend: "https://api.example.com/anthropic", Timeout: 300},
		},
	}
	hs := config.NewHealthStore(config.NewTestConfigStore(cfg), time.Minute, time.Second)
	SetHealthStore(hs)
	defer SetHealthStore(nil)

	online := func() bool {
		s, _ := hs.GetStatusForModel("m")
		return s.Online
	}

	// 2xx marks online.
	recordBackendHealth("m", "", 200)
	if !online() {
		t.Fatalf("200 should mark online")
	}

	// 402 (insufficient balance) marks offline with an explanatory error.
	recordBackendHealth("m", "", 402)
	if online() {
		t.Fatalf("402 should mark offline")
	}
	if s, _ := hs.GetStatusForModel("m"); !strings.Contains(s.Error, "credentials") {
		t.Errorf("402 error should mention credentials, got %q", s.Error)
	}

	// Recover to online, then confirm non-decisive codes do NOT flip it.
	recordBackendHealth("m", "", 200)
	for _, code := range []int{400, 404, 422, 429} {
		recordBackendHealth("m", "", code)
		if !online() {
			t.Errorf("HTTP %d should leave status unchanged (still online)", code)
		}
	}

	// 5xx marks offline.
	recordBackendHealth("m", "", 503)
	if online() {
		t.Fatalf("503 should mark offline")
	}

	// 401/403 mark offline.
	recordBackendHealth("m", "", 200)
	recordBackendHealth("m", "", 401)
	if online() {
		t.Fatalf("401 should mark offline")
	}

	// Unknown model and unset recorder must not panic.
	recordBackendHealth("does-not-exist", "", 200)
	SetHealthStore(nil)
	recordBackendHealth("m", "", 500)
}
