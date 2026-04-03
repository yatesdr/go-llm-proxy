package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/pipeline"
)

func testHandler(searchKey string) *Handler {
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{WebSearchKey: searchKey},
		Models:     []config.ModelConfig{{Name: "test", Backend: "http://localhost/v1"}},
	}
	cs := config.NewTestConfigStore(cfg)
	pl := pipeline.NewPipeline(cs, http.DefaultClient)
	return NewHandler(cs, pl)
}

// --- ServeSSE tests ---

func TestServeSSE_SendsEndpoint(t *testing.T) {
	h := testHandler("tvly-test")

	req := httptest.NewRequest("GET", "/mcp/sse", nil)
	w := httptest.NewRecorder()

	// Run in a goroutine since ServeSSE blocks.
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeSSE(w, req)
	}()

	// Wait briefly for the endpoint event to be written.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	body := w.Body.String()
	if !strings.Contains(body, "event: endpoint") {
		t.Fatalf("expected endpoint event, got: %s", body)
	}
	if !strings.Contains(body, "/mcp/messages?session=") {
		t.Fatalf("expected message URL in endpoint data, got: %s", body)
	}
}

func TestServeSSE_NoSearchKey(t *testing.T) {
	h := testHandler("")

	req := httptest.NewRequest("GET", "/mcp/sse", nil)
	w := httptest.NewRecorder()
	h.ServeSSE(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// --- ServeMessages tests ---

func startSession(t *testing.T, h *Handler) string {
	t.Helper()
	id := randomSessionID()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.sessions.Store(id, cancel)
	_ = ctx
	return id
}

func postMessage(t *testing.T, h *Handler, sessionID string, method string, params any) *httptest.ResponseRecorder {
	t.Helper()
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  method,
	}
	if params != nil {
		p, _ := json.Marshal(params)
		req.Params = p
	}
	body, _ := json.Marshal(req)
	httpReq := httptest.NewRequest("POST", "/mcp/messages?session="+sessionID, strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	h.ServeMessages(w, httpReq)
	return w
}

func TestServeMessages_Initialize(t *testing.T) {
	h := testHandler("tvly-test")
	sid := startSession(t, h)

	w := postMessage(t, h, sid, "initialize", nil)

	var resp jsonRPCResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	result := resp.Result.(map[string]any)
	if result["protocolVersion"] != "2024-11-05" {
		t.Fatalf("unexpected protocol version: %v", result["protocolVersion"])
	}
	serverInfo := result["serverInfo"].(map[string]any)
	if serverInfo["name"] != "go-llm-proxy" {
		t.Fatalf("unexpected server name: %v", serverInfo["name"])
	}
}

func TestServeMessages_ToolsList(t *testing.T) {
	h := testHandler("tvly-test")
	sid := startSession(t, h)

	w := postMessage(t, h, sid, "tools/list", nil)

	var resp jsonRPCResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	result := resp.Result.(map[string]any)
	tools := result["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	tool := tools[0].(map[string]any)
	if tool["name"] != "web_search" {
		t.Fatalf("expected web_search tool, got %v", tool["name"])
	}

	schema := tool["inputSchema"].(map[string]any)
	props := schema["properties"].(map[string]any)
	if _, ok := props["query"]; !ok {
		t.Fatal("expected query property in input schema")
	}
}

func TestServeMessages_ToolsListEmpty_NoSearchKey(t *testing.T) {
	h := testHandler("")
	sid := startSession(t, h)

	w := postMessage(t, h, sid, "tools/list", nil)

	var resp jsonRPCResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	result := resp.Result.(map[string]any)
	tools := result["tools"].([]any)
	if len(tools) != 0 {
		t.Fatalf("expected 0 tools without search key, got %d", len(tools))
	}
}

func TestServeMessages_ToolsCall_SearchError(t *testing.T) {
	h := testHandler("tvly-test")
	sid := startSession(t, h)

	// ExecuteTavilySearch will fail because the Tavily URL is hardcoded
	// and unreachable in tests. We verify the error is handled gracefully.
	w := postMessage(t, h, sid, "tools/call", map[string]any{
		"name":      "web_search",
		"arguments": map[string]any{"query": "test query"},
	})

	var resp jsonRPCResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Error != nil {
		t.Fatalf("expected result (not JSON-RPC error), got error: %v", resp.Error)
	}

	result := resp.Result.(map[string]any)
	if result["isError"] != true {
		t.Fatal("expected isError: true for failed search")
	}

	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "Web search failed") {
		t.Fatalf("expected failure message, got: %s", text)
	}
}

func TestServeMessages_ToolsCall_NoSearchKey(t *testing.T) {
	h := testHandler("")
	sid := startSession(t, h)

	w := postMessage(t, h, sid, "tools/call", map[string]any{
		"name":      "web_search",
		"arguments": map[string]any{"query": "test"},
	})

	var resp jsonRPCResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	result := resp.Result.(map[string]any)
	if result["isError"] != true {
		t.Fatal("expected isError: true when search key not configured")
	}
}

func TestServeMessages_ToolsCall_UnknownTool(t *testing.T) {
	h := testHandler("tvly-test")
	sid := startSession(t, h)

	w := postMessage(t, h, sid, "tools/call", map[string]any{
		"name":      "nonexistent",
		"arguments": map[string]any{},
	})

	var resp jsonRPCResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Error == nil {
		t.Fatal("expected JSON-RPC error for unknown tool")
	}
}

func TestServeMessages_ToolsCall_MissingQuery(t *testing.T) {
	h := testHandler("tvly-test")
	sid := startSession(t, h)

	w := postMessage(t, h, sid, "tools/call", map[string]any{
		"name":      "web_search",
		"arguments": map[string]any{},
	})

	var resp jsonRPCResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Error == nil {
		t.Fatal("expected JSON-RPC error for missing query")
	}
}

func TestServeMessages_InvalidSession(t *testing.T) {
	h := testHandler("tvly-test")

	w := postMessage(t, h, "nonexistent-session", "initialize", nil)

	var resp jsonRPCResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Error == nil {
		t.Fatal("expected error for unknown session")
	}
}

func TestServeMessages_MissingSession(t *testing.T) {
	h := testHandler("tvly-test")

	body, _ := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "initialize"})
	httpReq := httptest.NewRequest("POST", "/mcp/messages", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	h.ServeMessages(w, httpReq)

	var resp jsonRPCResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Error == nil {
		t.Fatal("expected error for missing session param")
	}
}

func TestServeMessages_UnknownMethod(t *testing.T) {
	h := testHandler("tvly-test")
	sid := startSession(t, h)

	w := postMessage(t, h, sid, "nonexistent/method", nil)

	var resp jsonRPCResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
}

func TestServeMessages_NotificationsInitialized(t *testing.T) {
	h := testHandler("tvly-test")
	sid := startSession(t, h)

	w := postMessage(t, h, sid, "notifications/initialized", nil)

	// Notifications return 204 No Content.
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
}

func TestServeSSE_SessionCleanup(t *testing.T) {
	h := testHandler("tvly-test")

	req := httptest.NewRequest("GET", "/mcp/sse", nil)
	w := httptest.NewRecorder()

	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeSSE(w, req)
	}()

	// Wait for session to be created, then find it in the sessions map.
	time.Sleep(50 * time.Millisecond)

	var sessionID string
	h.sessions.Range(func(key, value any) bool {
		sessionID = key.(string)
		return false // stop after first
	})
	if sessionID == "" {
		t.Fatal("no session found in sessions map")
	}

	// Cancel context (simulates client disconnect).
	cancel()
	<-done

	// Now safe to read w.Body — goroutine has exited.
	body := w.Body.String()
	if !strings.Contains(body, "event: endpoint") {
		t.Fatalf("expected endpoint event, got: %s", body)
	}

	// Session should be cleaned up.
	if _, ok := h.sessions.Load(sessionID); ok {
		t.Fatal("session should be cleaned up after disconnect")
	}
}
