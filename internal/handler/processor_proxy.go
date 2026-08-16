package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/auth"
	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
	"go-llm-proxy/internal/lb"
)

// AudioHandler exposes the standard OpenAI audio routes while keeping their
// capacity separate from chat models. If a route has not been configured in
// the dedicated audio section, requests fall back to the generic proxy so old
// model-based audio configurations continue working during migration.
type AudioHandler struct {
	config   *config.ConfigStore
	fallback http.Handler
}

func NewAudioHandler(cs *config.ConfigStore, fallback http.Handler) *AudioHandler {
	return &AudioHandler{config: cs, fallback: fallback}
}

func (h *AudioHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cfg := h.config.Get()
	var audio *config.AudioModelConfig
	switch r.URL.Path {
	case "/v1/audio/transcriptions", "/v1/audio/translations":
		audio = cfg.Audio.Whisper
	case "/v1/audio/speech":
		audio = cfg.Audio.TTS
	default:
		httputil.WriteError(w, http.StatusNotFound, "unsupported audio endpoint")
		return
	}
	if audio == nil {
		h.fallback.ServeHTTP(w, r)
		return
	}
	if r.Method != http.MethodPost {
		httputil.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, api.MaxRequestBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		httputil.WriteError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	contentType := r.Header.Get("Content-Type")
	isMultipart := strings.HasPrefix(contentType, "multipart/form-data")
	modelName := ExtractModelFromJSON(body)
	if isMultipart {
		modelName = ExtractModelFromMultipart(body, contentType)
	}
	if modelName == "" {
		httputil.WriteError(w, http.StatusBadRequest, "missing model field in request")
		return
	}
	if modelName != audio.Name {
		// A legacy audio model may still exist in the chat models list.
		r.Body = io.NopCloser(bytes.NewReader(body))
		h.fallback.ServeHTTP(w, r)
		return
	}
	if !auth.KeyAllowsModel(auth.KeyFromContext(r.Context()), audio.Name) {
		httputil.WriteError(w, http.StatusForbidden, "not authorized for requested model")
		return
	}
	if audio.Model != "" && audio.Model != audio.Name {
		if isMultipart {
			body = RewriteModelInMultipart(body, contentType, audio.Model)
		} else {
			body = RewriteModelName(body, audio.Model)
		}
	}

	upstreamPath := strings.TrimPrefix(r.URL.Path, "/v1")
	proxyWorkload(w, r, workloadRequest{
		Name: audio.Name, Backends: audio.Backends, Body: body,
		UpstreamPath: upstreamPath, Timeout: audio.Timeout, BackendType: config.BackendOpenAI,
	})
}

// DocumentHandler publishes the official PaddleOCR layout parsing contract at
// /layout-parsing. It does not invent a model ID or wrap the response in an
// OpenAI shape; clients receive result.layoutParsingResults[].markdown.text.
type DocumentHandler struct{ config *config.ConfigStore }

func NewDocumentHandler(cs *config.ConfigStore) *DocumentHandler { return &DocumentHandler{config: cs} }

func (h *DocumentHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httputil.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	doc := h.config.Get().Documents.PaddleOCR
	if doc == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "document processing is not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, api.MaxRequestBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		httputil.WriteError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	if len(bytes.TrimSpace(body)) == 0 {
		httputil.WriteError(w, http.StatusBadRequest, "request body is required")
		return
	}
	proxyWorkload(w, r, workloadRequest{
		Name: "paddleocr", Backends: doc.Backends, Body: body,
		UpstreamPath: doc.Endpoint, Timeout: doc.Timeout, BackendType: config.BackendOpenAI,
	})
}

type workloadRequest struct {
	Name, UpstreamPath, BackendType string
	Backends                        []config.BackendConfig
	Body                            []byte
	Timeout                         int
}

func proxyWorkload(w http.ResponseWriter, r *http.Request, wr workloadRequest) {
	selected, release := lb.ResolveBackends(wr.Backends, 0)
	if selected == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "no backend is configured")
		return
	}
	defer release()

	if wr.Timeout <= 0 {
		wr.Timeout = 300
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(wr.Timeout)*time.Second)
	defer cancel()
	client := httputil.NewHTTPClientWithResponseHeaderTimeout(time.Duration(wr.Timeout) * time.Second)
	resp, err := doWorkloadRequest(ctx, client, r, selected, wr)
	if (err != nil && ctx.Err() == nil) || (err == nil && resp.StatusCode >= 500) {
		lb.RecordOutcome(selected.URL, false)
		if alternate, altRelease := lb.ResolveAlternateBackend(wr.Backends, selected.URL); alternate != nil {
			defer altRelease()
			if resp != nil {
				resp.Body.Close()
			}
			slog.Warn("processor failover", "workload", wr.Name, "failed", selected.URL, "alternate", alternate.URL)
			selected = alternate
			resp, err = doWorkloadRequest(ctx, client, r, selected, wr)
		}
	}
	if err != nil {
		lb.RecordOutcome(selected.URL, false)
		if ctx.Err() != nil {
			httputil.WriteError(w, http.StatusGatewayTimeout, "upstream request timed out")
		} else {
			httputil.WriteError(w, http.StatusBadGateway, "upstream request failed")
		}
		return
	}
	defer resp.Body.Close()
	lb.RecordOutcome(selected.URL, resp.StatusCode < 500)

	for _, header := range []string{"Content-Type", "Content-Disposition", "X-Request-ID", "Request-Id"} {
		if value := resp.Header.Get(header); value != "" {
			w.Header().Set(header, value)
		}
	}
	httputil.SetSecurityHeaders(w)
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, io.LimitReader(resp.Body, api.MaxResponseBodySize)); err != nil {
		slog.Warn("processor response interrupted", "workload", wr.Name, "error", err)
	}
}

func doWorkloadRequest(ctx context.Context, client *http.Client, original *http.Request, backend *config.BackendConfig, wr workloadRequest) (*http.Response, error) {
	upstreamURL := strings.TrimRight(backend.URL, "/") + "/" + strings.TrimLeft(wr.UpstreamPath, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(wr.Body))
	if err != nil {
		return nil, fmt.Errorf("create upstream request: %w", err)
	}
	for _, header := range []string{"Accept", "Content-Type", "X-Request-ID"} {
		if value := original.Header.Get(header); value != "" {
			req.Header.Set(header, value)
		}
	}
	if backend.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+backend.APIKey)
	}
	return client.Do(req)
}
