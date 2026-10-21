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

func TestDocumentHandlerRoutesPipelineEndpoints(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		io.WriteString(w, `{"result":{}}`)
	}))
	defer upstream.Close()

	tests := []struct{ endpoint, public, want string }{
		{"/layout-parsing", "/layout-parsing", "/layout-parsing"},
		{"/layout-parsing", "/v1/layout-parsing", "/layout-parsing"},
		{"/layout-parsing", "/restructure-pages", "/restructure-pages"},
		{"/layout-parsing", "/v1/restructure-pages", "/restructure-pages"},
		{"/ocr/layout-parsing", "/restructure-pages", "/ocr/restructure-pages"},
	}
	for _, tt := range tests {
		cs := config.NewTestConfigStore(&config.Config{Documents: config.DocumentsConfig{PaddleOCR: &config.PaddleOCRConfig{
			Endpoint: tt.endpoint, Timeout: 10, Backends: []config.BackendConfig{{URL: upstream.URL}},
		}}})
		r := httptest.NewRequest(http.MethodPost, tt.public, strings.NewReader(`{"pages":[]}`))
		w := httptest.NewRecorder()
		NewDocumentHandler(cs).ServeHTTP(w, r)
		if w.Code != http.StatusOK || gotPath != tt.want {
			t.Errorf("endpoint %s, POST %s: status %d, upstream path %q, want %q", tt.endpoint, tt.public, w.Code, gotPath, tt.want)
		}
	}
}

func TestDefaultInputImageDetail(t *testing.T) {
	in := `{"model":"m","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AA=="},{"type":"input_image","image_url":"https://x/a.png?a=1&b=2","detail":"low"},{"type":"input_text","text":"<hi>"}]}],"top_logprobs":3}`
	var got struct {
		Input []struct {
			Content []map[string]any `json:"content"`
		} `json:"input"`
		TopLogprobs json.Number `json:"top_logprobs"`
	}
	out := defaultInputImageDetail([]byte(in))
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output does not parse: %v\n%s", err, out)
	}
	parts := got.Input[0].Content
	if parts[0]["detail"] != "auto" || parts[1]["detail"] != "low" || got.TopLogprobs != "3" {
		t.Fatalf("unexpected rewrite: %s", out)
	}
	if strings.Contains(string(out), `\u0026`) || strings.Contains(string(out), `\u003c`) {
		t.Fatalf("output was HTML-escaped: %s", out)
	}

	for _, unchanged := range []string{
		`{"model":"m","input":"hi"}`,
		`{"model":"m","input":[{"type":"input_image","image_url":"u","detail":"high"}]}`,
		`not json "input_image"`,
	} {
		if got := string(defaultInputImageDetail([]byte(unchanged))); got != unchanged {
			t.Errorf("body changed: %s -> %s", unchanged, got)
		}
	}
}

func TestProxyPassesCleanedBackendErrorMessage(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"\"auto\" tool choice requires --enable-auto-tool-choice (see http://10.0.0.5:8000/docs)","type":"BadRequestError"}}`)
	}))
	defer backend.Close()

	p := newProxyForModel(config.ModelConfig{Name: "m", Model: "m", Backend: backend.URL + "/v1", Timeout: 10})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	var got struct {
		Error struct{ Message, Code, Type string } `json:"error"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusBadRequest || got.Error.Code != "invalid_prompt" || !strings.Contains(got.Error.Message, "--enable-auto-tool-choice") {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	if strings.Contains(got.Error.Message, "10.0.0.5") || strings.Contains(got.Error.Message, "http://") {
		t.Fatalf("backend address leaked: %s", got.Error.Message)
	}
}

func TestProxyMarksContextLengthErrors(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"This model's maximum context length is 16384 tokens."}}`)
	}))
	defer backend.Close()

	p := newProxyForModel(config.ModelConfig{Name: "m", Model: "m", Backend: backend.URL + "/v1", Timeout: 10})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"context_length_exceeded"`) {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
}
