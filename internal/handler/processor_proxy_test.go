package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go-llm-proxy/internal/config"
)

func TestDocumentHandlerPreservesPaddleContract(t *testing.T) {
	var gotAuth, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"file":"abc"`) {
			t.Errorf("request body changed: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"result":{"layoutParsingResults":[{"markdown":{"text":"table"}}]},"errorCode":0}`)
	}))
	defer upstream.Close()

	cs := config.NewTestConfigStore(&config.Config{Documents: config.DocumentsConfig{PaddleOCR: &config.PaddleOCRConfig{
		Endpoint: "/layout-parsing", Timeout: 10,
		Backends: []config.BackendConfig{{URL: upstream.URL, APIKey: "processor-secret"}},
	}}})
	h := NewDocumentHandler(cs)
	r := httptest.NewRequest(http.MethodPost, "/layout-parsing", strings.NewReader(`{"file":"abc"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotPath != "/layout-parsing" || gotAuth != "Bearer processor-secret" {
		t.Fatalf("path/auth mismatch: %q %q", gotPath, gotAuth)
	}
	if !strings.Contains(w.Body.String(), "layoutParsingResults") {
		t.Fatalf("Paddle response shape changed: %s", w.Body.String())
	}
}

func TestAudioHandlerRoutesAndRewritesTTSModel(t *testing.T) {
	var gotModel, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write([]byte("audio"))
	}))
	defer upstream.Close()

	cs := config.NewTestConfigStore(&config.Config{Audio: config.AudioConfig{TTS: &config.AudioModelConfig{
		Name: "speech", Model: "upstream-tts", Timeout: 10,
		Backends: []config.BackendConfig{{URL: upstream.URL + "/v1"}},
	}}})
	h := NewAudioHandler(cs, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unexpected legacy fallback")
	}))
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"speech","input":"hello","voice":"alloy"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != "audio" {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if gotPath != "/v1/audio/speech" || gotModel != "upstream-tts" {
		t.Fatalf("route/model mismatch: path=%q model=%q", gotPath, gotModel)
	}
}

func TestAudioHandlerFallsBackForLegacyModel(t *testing.T) {
	cs := config.NewTestConfigStore(&config.Config{Audio: config.AudioConfig{TTS: &config.AudioModelConfig{
		Name: "new-tts", Backends: []config.BackendConfig{{URL: "http://unused/v1"}},
	}}})
	called := false
	h := NewAudioHandler(cs, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"legacy"`) {
			t.Errorf("fallback body not restored: %s", body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"legacy"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !called || w.Code != http.StatusNoContent {
		t.Fatalf("fallback not used: called=%v status=%d", called, w.Code)
	}
}
