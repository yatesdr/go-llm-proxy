package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
)

// maxRequestBodySize limits request body to 50 MB (covers base64 images for vision).
const maxRequestBodySize = 50 * 1024 * 1024

// allowedPaths restricts which sub-paths can be proxied to backends.
var allowedPaths = regexp.MustCompile(`^/v1/(chat/completions|completions|embeddings|images/generations|audio/(transcriptions|translations|speech))$`)

// allowedResponseHeaders controls which upstream headers are forwarded to clients.
var allowedResponseHeaders = map[string]bool{
	"Content-Type":          true,
	"Content-Length":        true,
	"X-Request-ID":         true,
	"Openai-Processing-Ms": true,
	"Openai-Model":         true,
}

type ProxyHandler struct {
	config *ConfigStore
	client *http.Client
}

func NewProxyHandler(cs *ConfigStore) *ProxyHandler {
	return &ProxyHandler{
		config: cs,
		client: &http.Client{
			// Do not follow redirects — prevents SSRF via backend redirects.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (p *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only allow POST — all proxied endpoints are POST.
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Validate the request path against the allowlist.
	cleanPath := path.Clean(r.URL.Path)
	if !allowedPaths.MatchString(cleanPath) {
		writeError(w, http.StatusNotFound, "unsupported endpoint")
		return
	}

	// Limit request body size to prevent memory exhaustion.
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}

	// Detect content type for model extraction strategy.
	contentType := r.Header.Get("Content-Type")
	isMultipart := strings.HasPrefix(contentType, "multipart/form-data")

	var modelName string
	if isMultipart {
		modelName = extractModelFromMultipart(body, contentType)
	} else {
		modelName = extractModelFromJSON(body)
	}
	if modelName == "" {
		writeError(w, http.StatusBadRequest, "missing model field in request")
		return
	}

	// Snapshot config once to avoid race on reload.
	cfg := p.config.Get()

	// Check key authorization for this model.
	key := keyFromContext(r.Context())
	if !keyAllowsModel(key, modelName) {
		writeError(w, http.StatusForbidden, "not authorized for requested model")
		return
	}

	model := findModel(cfg, modelName)
	if model == nil {
		writeError(w, http.StatusNotFound, "unknown model")
		return
	}

	// Rewrite the model name in the body if the backend expects a different name.
	if model.Model != modelName {
		if isMultipart {
			body = rewriteModelInMultipart(body, contentType, model.Model)
		} else {
			body = rewriteModelName(body, model.Model)
		}
	}

	// Build the upstream URL. Strip the /v1 prefix since backends include their own version path.
	relPath := strings.TrimPrefix(cleanPath, "/v1")
	upstreamURL := strings.TrimRight(model.Backend, "/") + relPath

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(model.Timeout)*time.Second)
	defer cancel()

	upReq, err := http.NewRequestWithContext(ctx, r.Method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create upstream request")
		return
	}

	// Copy only specific headers from the client.
	copyHeaders(upReq.Header, r.Header)

	// Set the backend API key if configured.
	if model.APIKey != "" {
		upReq.Header.Set("Authorization", "Bearer "+model.APIKey)
	}

	keyName := ""
	if key != nil {
		keyName = key.Name
	}
	slog.Info("proxying request",
		"model", modelName,
		"path", cleanPath,
		"key", keyName,
	)

	resp, err := p.client.Do(upReq)
	if err != nil {
		if ctx.Err() != nil {
			writeError(w, http.StatusGatewayTimeout, "upstream request timed out")
			return
		}
		slog.Error("upstream request failed", "error", err, "model", modelName)
		writeError(w, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer resp.Body.Close()

	// Copy only allowed response headers.
	for k := range allowedResponseHeaders {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}

	setSecurityHeaders(w)

	w.WriteHeader(resp.StatusCode)

	// Stream the response, flushing after each read for SSE support.
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			break
		}
	}
}

func findModel(cfg *Config, name string) *ModelConfig {
	for i := range cfg.Models {
		if cfg.Models[i].Name == name {
			return &cfg.Models[i]
		}
	}
	return nil
}

// extractModelFromJSON pulls the model name from a JSON request body.
func extractModelFromJSON(body []byte) string {
	var req struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &req) == nil {
		return req.Model
	}
	return ""
}

// extractModelFromMultipart pulls the model name from a multipart/form-data body.
func extractModelFromMultipart(body []byte, contentType string) string {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	boundary := params["boundary"]
	if boundary == "" {
		return ""
	}

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err != nil {
			break
		}
		if part.FormName() == "model" {
			val, err := io.ReadAll(part)
			part.Close()
			if err == nil {
				return strings.TrimSpace(string(val))
			}
			break
		}
		part.Close()
	}
	return ""
}

// rewriteModelName replaces the "model" field in a JSON body. Other field values
// are preserved as raw bytes via json.RawMessage, but top-level key order may change.
func rewriteModelName(body []byte, newName string) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	if _, ok := m["model"]; !ok {
		return body
	}
	nameBytes, err := json.Marshal(newName)
	if err != nil {
		return body
	}
	m["model"] = json.RawMessage(nameBytes)
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// rewriteModelInMultipart rebuilds a multipart body with the model field replaced.
func rewriteModelInMultipart(body []byte, contentType string, newModel string) []byte {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return body
	}
	boundary := params["boundary"]
	if boundary == "" {
		return body
	}

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	writer.SetBoundary(boundary) // preserve original boundary so Content-Type header stays valid

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err != nil {
			break
		}

		if part.FormName() == "model" {
			// Replace model value.
			fw, err := writer.CreateFormField("model")
			if err != nil {
				part.Close()
				return body
			}
			fw.Write([]byte(newModel))
			part.Close()
			continue
		}

		// Copy other parts as-is.
		header := part.Header
		pw, err := writer.CreatePart(header)
		if err != nil {
			part.Close()
			return body
		}
		io.Copy(pw, part)
		part.Close()
	}
	writer.Close()
	return buf.Bytes()
}

func copyHeaders(dst, src http.Header) {
	forward := []string{
		"Accept",
		"Content-Type",
		"X-Request-ID",
	}
	for _, h := range forward {
		if v := src.Get(h); v != "" {
			dst.Set(h, v)
		}
	}
}
