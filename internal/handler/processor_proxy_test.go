package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go-llm-proxy/internal/auth"
	"go-llm-proxy/internal/config"
)

type countingFlushRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (w *countingFlushRecorder) Flush() {
	w.flushes++
	w.ResponseRecorder.Flush()
}

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

func TestAudioHandlerFlushesStreamingAudioAndDisablesNginxBuffering(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/pcm")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("first"))
		w.(http.Flusher).Flush()
		w.Write([]byte("second"))
	}))
	defer upstream.Close()

	cs := config.NewTestConfigStore(&config.Config{Audio: config.AudioConfig{TTS: &config.AudioModelConfig{
		Name: "kokoro", Timeout: 10, Backends: []config.BackendConfig{{URL: upstream.URL + "/v1"}},
	}}})
	h := NewAudioHandler(cs, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unexpected legacy fallback")
	}))
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"kokoro","input":"hello","stream":true,"response_format":"pcm"}`))
	r.Header.Set("Content-Type", "application/json")
	w := &countingFlushRecorder{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK || w.Body.String() != "firstsecond" {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering=%q", got)
	}
	if w.flushes < 2 {
		t.Fatalf("flushes=%d want at least header and body flushes", w.flushes)
	}
}

func TestAudioHandlerPropagatesClientCancellation(t *testing.T) {
	upstreamStarted := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/pcm")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(upstreamStarted)
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer upstream.Close()

	cs := config.NewTestConfigStore(&config.Config{Audio: config.AudioConfig{TTS: &config.AudioModelConfig{
		Name: "kokoro", Timeout: 10, Backends: []config.BackendConfig{{URL: upstream.URL + "/v1"}},
	}}})
	h := NewAudioHandler(cs, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unexpected legacy fallback")
	}))
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"kokoro","input":"hello","stream":true,"response_format":"pcm"}`)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(w, r)
		close(done)
	}()

	select {
	case <-upstreamStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not start")
	}
	cancel()
	select {
	case <-upstreamCanceled:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request was not canceled")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("proxy handler did not return after cancellation")
	}
}

func TestAudioHandlerRoutesTTSVoiceDiscovery(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"voices":[{"id":"af_heart","name":"af_heart"}]}`)
	}))
	defer upstream.Close()

	cs := config.NewTestConfigStore(&config.Config{Audio: config.AudioConfig{TTS: &config.AudioModelConfig{
		Name: "speech", Timeout: 10,
		Backends: []config.BackendConfig{{URL: upstream.URL + "/v1", APIKey: "tts-secret"}},
	}}})
	h := NewAudioHandler(cs, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unexpected legacy fallback")
	}))
	r := httptest.NewRequest(http.MethodGet, "/v1/audio/voices", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotMethod != http.MethodGet || gotPath != "/v1/audio/voices" || gotAuth != "Bearer tts-secret" {
		t.Fatalf("upstream mismatch: method=%q path=%q auth=%q", gotMethod, gotPath, gotAuth)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type=%q", got)
	}
	if got := w.Body.String(); got != `{"voices":[{"id":"af_heart","name":"af_heart"}]}` {
		t.Fatalf("response body changed: %s", got)
	}
}

func TestAudioHandlerVoiceDiscoveryRequiresConfiguredTTS(t *testing.T) {
	cs := config.NewTestConfigStore(&config.Config{})
	h := NewAudioHandler(cs, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("voice discovery must not use the legacy fallback")
	}))
	r := httptest.NewRequest(http.MethodGet, "/v1/audio/voices", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "text-to-speech is not configured") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestAudioHandlerVoiceDiscoveryEnforcesTTSModelACL(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cs := config.NewTestConfigStore(&config.Config{
		Keys: []config.KeyConfig{{Key: "restricted-key", Name: "restricted", Models: []string{"chat"}}},
		Audio: config.AudioConfig{TTS: &config.AudioModelConfig{
			Name: "speech", Timeout: 10, Backends: []config.BackendConfig{{URL: upstream.URL + "/v1"}},
		}},
	})
	audio := NewAudioHandler(cs, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unexpected legacy fallback")
	}))
	h := auth.AuthMiddleware(cs, audio)
	r := httptest.NewRequest(http.MethodGet, "/v1/audio/voices", nil)
	r.Header.Set("Authorization", "Bearer restricted-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if called {
		t.Fatal("unauthorized voice discovery reached the upstream")
	}
}

func TestAudioHandlerVoiceDiscoveryRejectsPOST(t *testing.T) {
	cs := config.NewTestConfigStore(&config.Config{})
	h := NewAudioHandler(cs, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unexpected legacy fallback")
	}))
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/voices", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
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
