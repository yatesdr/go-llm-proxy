// Package mcp implements an MCP (Model Context Protocol) SSE server that
// exposes the proxy's web search capability as an MCP tool. This allows
// MCP-capable clients (OpenCode, Qwen Code) to use proxy-side Tavily search
// without needing their own API key.
package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/pipeline"
)

// Handler serves MCP SSE transport endpoints.
type Handler struct {
	config   *config.ConfigStore
	pipeline *pipeline.Pipeline
	sessions sync.Map // sessionID → context.CancelFunc
}

// NewHandler creates an MCP handler that uses the given config and pipeline
// for executing web search.
func NewHandler(cs *config.ConfigStore, pl *pipeline.Pipeline) *Handler {
	return &Handler{config: cs, pipeline: pl}
}

// --- JSON-RPC types ---

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Result  any    `json:"result,omitempty"`
	Error   any    `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// --- SSE endpoint ---

// ServeSSE handles GET /mcp/sse — establishes an SSE connection and sends
// the message endpoint URL. The connection stays open for the session lifetime.
func (h *Handler) ServeSSE(w http.ResponseWriter, r *http.Request) {
	// MCP is only available when web search is configured.
	cfg := h.config.Get()
	if cfg.Processors.WebSearchKey == "" {
		http.Error(w, "MCP not available: no web_search_key configured", http.StatusNotFound)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	sessionID := randomSessionID()
	ctx, cancel := context.WithCancel(r.Context())
	h.sessions.Store(sessionID, cancel)

	defer func() {
		h.sessions.Delete(sessionID)
		cancel()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Send the endpoint event so the client knows where to POST messages.
	fmt.Fprintf(w, "event: endpoint\ndata: /mcp/messages?session=%s\n\n", sessionID)
	flusher.Flush()

	slog.Info("MCP session started", "session", sessionID)

	// Hold connection open with keepalives until the client disconnects.
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("MCP session ended", "session", sessionID)
			return
		case <-ticker.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// --- Messages endpoint ---

const maxMCPBodySize = 1 << 20 // 1 MB

// ServeMessages handles POST /mcp/messages?session=<id> — receives JSON-RPC
// requests and returns JSON-RPC responses.
func (h *Handler) ServeMessages(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		writeJSONRPCError(w, nil, -32600, "missing session parameter")
		return
	}
	if _, ok := h.sessions.Load(sessionID); !ok {
		writeJSONRPCError(w, nil, -32600, "unknown or expired session")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxMCPBodySize))
	r.Body.Close()
	if err != nil {
		writeJSONRPCError(w, nil, -32700, "failed to read request body")
		return
	}

	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONRPCError(w, nil, -32700, "invalid JSON")
		return
	}

	slog.Debug("MCP message received", "session", sessionID, "method", req.Method)

	switch req.Method {
	case "initialize":
		h.handleInitialize(w, req)
	case "notifications/initialized":
		// Notification — no response required.
		w.WriteHeader(http.StatusNoContent)
	case "tools/list":
		h.handleToolsList(w, req)
	case "tools/call":
		h.handleToolsCall(w, r.Context(), req)
	default:
		writeJSONRPCError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

func (h *Handler) handleInitialize(w http.ResponseWriter, req jsonRPCRequest) {
	writeJSONRPCResult(w, req.ID, map[string]any{
		"protocolVersion": "2024-11-05",
		"serverInfo": map[string]any{
			"name":    "go-llm-proxy",
			"version": "1.0.0",
		},
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
	})
}

func (h *Handler) handleToolsList(w http.ResponseWriter, req jsonRPCRequest) {
	tools := []any{}

	cfg := h.config.Get()
	if cfg.Processors.WebSearchKey != "" {
		tools = append(tools, map[string]any{
			"name":        "web_search",
			"description": "Search the web for current information. Use when the user asks about recent events, current data, or anything that requires up-to-date information.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Search query",
					},
				},
				"required": []string{"query"},
			},
		})
	}

	writeJSONRPCResult(w, req.ID, map[string]any{
		"tools": tools,
	})
}

func (h *Handler) handleToolsCall(w http.ResponseWriter, ctx context.Context, req jsonRPCRequest) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeJSONRPCError(w, req.ID, -32602, "invalid params: "+err.Error())
		return
	}

	if params.Name != "web_search" {
		writeJSONRPCError(w, req.ID, -32602, "unknown tool: "+params.Name)
		return
	}

	query, _ := params.Arguments["query"].(string)
	if query == "" {
		writeJSONRPCError(w, req.ID, -32602, "missing required argument: query")
		return
	}

	searchKey := h.config.Get().Processors.WebSearchKey
	if searchKey == "" {
		writeJSONRPCResult(w, req.ID, map[string]any{
			"content": []any{map[string]any{
				"type": "text",
				"text": "Web search is not configured on this proxy.",
			}},
			"isError": true,
		})
		return
	}

	slog.Debug("MCP web_search call", "query", query)

	result, err := h.pipeline.ExecuteTavilySearch(ctx, searchKey, query)
	if err != nil {
		slog.Warn("MCP web_search failed", "query", query, "error", err)
		writeJSONRPCResult(w, req.ID, map[string]any{
			"content": []any{map[string]any{
				"type": "text",
				"text": fmt.Sprintf("Web search failed: %s", err.Error()),
			}},
			"isError": true,
		})
		return
	}

	writeJSONRPCResult(w, req.ID, map[string]any{
		"content": []any{map[string]any{
			"type": "text",
			"text": result,
		}},
	})
}

// --- Helpers ---

func writeJSONRPCResult(w http.ResponseWriter, id any, result any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	})
}

func writeJSONRPCError(w http.ResponseWriter, id any, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   jsonRPCError{Code: code, Message: message},
	})
}

func randomSessionID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
