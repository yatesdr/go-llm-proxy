package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go-llm-proxy/internal/auth"
	"go-llm-proxy/internal/config"
)

func discoveredModelIDs(t *testing.T, h http.Handler, key string) []string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode models response: %v", err)
	}
	ids := make([]string, 0, len(response.Data))
	for _, model := range response.Data {
		ids = append(ids, model.ID)
	}
	return ids
}

func TestModelsHandlerIncludesConfiguredAudioWorkloads(t *testing.T) {
	cs := config.NewTestConfigStore(&config.Config{
		Models: []config.ModelConfig{{Name: "chat", ContextWindow: 32768}},
		Audio: config.AudioConfig{
			Whisper: &config.AudioModelConfig{Name: "whisper-large-v3"},
			TTS:     &config.AudioModelConfig{Name: "kokoro"},
		},
	})
	h := NewModelsHandler(cs, nil)

	got := discoveredModelIDs(t, h, "")
	want := []string{"chat", "whisper-large-v3", "kokoro"}
	if len(got) != len(want) {
		t.Fatalf("model ids=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("model ids=%v want=%v", got, want)
		}
	}
}

func TestModelsHandlerAppliesModelACLToAudioWorkloads(t *testing.T) {
	cs := config.NewTestConfigStore(&config.Config{
		Models: []config.ModelConfig{{Name: "chat"}},
		Audio: config.AudioConfig{
			Whisper: &config.AudioModelConfig{Name: "whisper-large-v3"},
			TTS:     &config.AudioModelConfig{Name: "kokoro"},
		},
		Keys: []config.KeyConfig{{Key: "tts-key", Models: []string{"kokoro"}}},
	})
	models := NewModelsHandler(cs, nil)
	h := auth.AuthMiddleware(cs, models)

	got := discoveredModelIDs(t, h, "tts-key")
	if len(got) != 1 || got[0] != "kokoro" {
		t.Fatalf("model ids=%v want=[kokoro]", got)
	}
}

func TestModelsHandlerIncludesAliasesUsingTargetACLAndContext(t *testing.T) {
	cs := config.NewTestConfigStore(&config.Config{
		Models: []config.ModelConfig{
			{Name: "allowed", ContextWindow: 131072},
			{Name: "denied", ContextWindow: 8192},
		},
		Aliases: map[string]string{"reviewer": "allowed", "hidden": "denied"},
		Keys:    []config.KeyConfig{{Key: "limited", Models: []string{"allowed"}}},
	})
	h := auth.AuthMiddleware(cs, NewModelsHandler(cs, nil))
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer limited")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	var response struct {
		Data []struct {
			ID            string `json:"id"`
			ContextWindow int    `json:"context_window"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Data) != 2 || response.Data[0].ID != "allowed" || response.Data[1].ID != "reviewer" {
		t.Fatalf("models = %+v", response.Data)
	}
	if response.Data[1].ContextWindow != 131072 {
		t.Fatalf("alias context window = %d", response.Data[1].ContextWindow)
	}
}
