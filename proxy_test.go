package main

import (
	"encoding/json"
	"testing"
)

func TestExtractModelFromJSON_Valid(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[]}`)
	if got := extractModelFromJSON(body); got != "gpt-4" {
		t.Fatalf("expected gpt-4, got: %q", got)
	}
}

func TestExtractModelFromJSON_Missing(t *testing.T) {
	body := []byte(`{"messages":[]}`)
	if got := extractModelFromJSON(body); got != "" {
		t.Fatalf("expected empty, got: %q", got)
	}
}

func TestExtractModelFromJSON_Invalid(t *testing.T) {
	body := []byte(`not json`)
	if got := extractModelFromJSON(body); got != "" {
		t.Fatalf("expected empty for invalid JSON, got: %q", got)
	}
}

func TestExtractModelFromJSON_Empty(t *testing.T) {
	body := []byte(`{}`)
	if got := extractModelFromJSON(body); got != "" {
		t.Fatalf("expected empty for empty object, got: %q", got)
	}
}

func TestRewriteModelName_Replaces(t *testing.T) {
	body := []byte(`{"model":"old-model","temperature":0.7}`)
	result := rewriteModelName(body, "new-model")

	var m map[string]json.RawMessage
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	var model string
	if err := json.Unmarshal(m["model"], &model); err != nil {
		t.Fatalf("failed to unmarshal model: %v", err)
	}
	if model != "new-model" {
		t.Fatalf("expected new-model, got: %q", model)
	}
}

func TestRewriteModelName_PreservesOtherFields(t *testing.T) {
	body := []byte(`{"model":"old","temperature":0.7,"max_tokens":100}`)
	result := rewriteModelName(body, "new")

	var m map[string]json.RawMessage
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	if _, ok := m["temperature"]; !ok {
		t.Fatal("temperature field was lost")
	}
	if _, ok := m["max_tokens"]; !ok {
		t.Fatal("max_tokens field was lost")
	}
}

func TestRewriteModelName_InvalidJSON(t *testing.T) {
	body := []byte(`not json`)
	result := rewriteModelName(body, "new")
	if string(result) != string(body) {
		t.Fatalf("expected original body returned for invalid JSON")
	}
}

func TestRewriteModelName_NoModelField(t *testing.T) {
	body := []byte(`{"temperature":0.7}`)
	result := rewriteModelName(body, "new")
	if string(result) != string(body) {
		t.Fatalf("expected original body when no model field present")
	}
}

func TestFindModel_Found(t *testing.T) {
	cfg := &Config{
		Models: []ModelConfig{
			{Name: "model-a", Backend: "http://localhost/v1"},
			{Name: "model-b", Backend: "http://localhost/v1"},
		},
	}

	m := findModel(cfg, "model-b")
	if m == nil || m.Name != "model-b" {
		t.Fatalf("expected model-b, got: %v", m)
	}
}

func TestFindModel_NotFound(t *testing.T) {
	cfg := &Config{
		Models: []ModelConfig{
			{Name: "model-a", Backend: "http://localhost/v1"},
		},
	}

	m := findModel(cfg, "nonexistent")
	if m != nil {
		t.Fatalf("expected nil, got: %v", m)
	}
}

func TestAllowedPaths(t *testing.T) {
	allowed := []string{
		"/v1/chat/completions",
		"/v1/completions",
		"/v1/embeddings",
		"/v1/images/generations",
		"/v1/audio/transcriptions",
		"/v1/audio/translations",
		"/v1/audio/speech",
	}
	for _, p := range allowed {
		if !allowedPaths.MatchString(p) {
			t.Errorf("expected %q to be allowed", p)
		}
	}

	denied := []string{
		"/v1/models",
		"/v1/fine-tunes",
		"/v1/chat/completions/extra",
		"/v2/chat/completions",
		"/v1/",
		"/",
	}
	for _, p := range denied {
		if allowedPaths.MatchString(p) {
			t.Errorf("expected %q to be denied", p)
		}
	}
}
