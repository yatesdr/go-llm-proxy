package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/config"
)

const webSearchFunctionName = "web_search"

// newWebSearchToolDef returns a fresh function tool definition for web search.
// Returns a new map each time to prevent concurrent mutation of shared state.
func newWebSearchToolDef() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        webSearchFunctionName,
			"description": "Search the web for current information. Use when the user asks about recent events, current data, or anything that requires up-to-date information.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Search query",
					},
				},
				"required": []string{"query"},
			},
		},
	}
}

// isSearchServerTool returns true if the tool type is a web search server tool
// from any supported client (Claude Code, Codex, etc.).
func isSearchServerTool(toolType string) bool {
	switch toolType {
	case "web_search_20250305", // Claude Code (Anthropic)
		"web_search_preview",     // Codex (OpenAI Responses API)
		"web_search_preview_2025_03_11": // Codex alternate
		return true
	}
	return false
}

// toolsContainName checks if a tools array (either []any or []map[string]any)
// contains a function tool with the given name.
func toolsContainName(tools any, name string) bool {
	check := func(tm map[string]any) bool {
		fn, ok := tm["function"].(map[string]any)
		return ok && fn["name"] == name
	}
	switch t := tools.(type) {
	case []any:
		for _, item := range t {
			if tm, ok := item.(map[string]any); ok && check(tm) {
				return true
			}
		}
	case []map[string]any:
		for _, tm := range t {
			if check(tm) {
				return true
			}
		}
	}
	return false
}

// convertOrInjectSearchTool handles web search for a translated Chat Completions request:
// 1. If server tools (web_search_20250305, web_search_preview) were stripped during
//    translation, re-inject as a regular function tool.
// 2. Ownership: does NOT delete _stripped_server_tools — ProcessRequest owns that cleanup.
func (p *Pipeline) convertOrInjectSearchTool(chatReq map[string]any, targetModel *config.ModelConfig) map[string]any {
	searchKey := p.ResolveWebSearchKey(targetModel)
	if searchKey == "" {
		return chatReq
	}

	// Check if server tools were stripped during translation.
	var hasStrippedSearch bool
	if stripped, ok := chatReq[InternalKeyStrippedTools].([]string); ok {
		for _, t := range stripped {
			if isSearchServerTool(t) {
				hasStrippedSearch = true
				break
			}
		}
	}

	if !hasStrippedSearch {
		return chatReq
	}

	// Don't duplicate if web_search already exists.
	if toolsContainName(chatReq["tools"], webSearchFunctionName) {
		return chatReq
	}

	slog.Debug("converting stripped search server tool to function tool")

	// Normalize tools to []any for consistent appending.
	switch tools := chatReq["tools"].(type) {
	case []map[string]any:
		anyTools := make([]any, len(tools))
		for i, t := range tools {
			anyTools[i] = t
		}
		chatReq["tools"] = append(anyTools, newWebSearchToolDef())
	case []any:
		chatReq["tools"] = append(tools, newWebSearchToolDef())
	default:
		chatReq["tools"] = []any{newWebSearchToolDef()}
	}

	return chatReq
}

// HasSearchToolCall checks if any of the tool calls are for web_search.
func HasSearchToolCall(toolCalls []api.ChatChoiceToolCall) bool {
	for _, tc := range toolCalls {
		if tc.Function.Name == webSearchFunctionName {
			return true
		}
	}
	return false
}

// --- Tavily API ---

// ExecuteTavilySearch calls the Tavily search API and returns formatted results.
// Uses the Pipeline's HTTP client for consistent redirect/timeout behavior.
func (p *Pipeline) ExecuteTavilySearch(ctx context.Context, apiKey, query string) (string, error) {
	start := time.Now()

	// 10-second timeout for Tavily calls.
	tavilyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	reqBody, err := json.Marshal(map[string]any{
		"api_key":        apiKey,
		"query":          query,
		"search_depth":   "basic",
		"include_answer": true,
		"max_results":    5,
	})
	if err != nil {
		return "", fmt.Errorf("marshal tavily request: %w", err)
	}

	req, err := http.NewRequestWithContext(tavilyCtx, "POST", "https://api.tavily.com/search", bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("build tavily request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("tavily request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB limit
	if err != nil {
		return "", fmt.Errorf("read tavily response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Log the full response for debugging but return a sanitized error to the caller.
		// The response body could contain reflected request data (including the API key).
		slog.Error("tavily API error", "status", resp.StatusCode, "body", string(body))
		return "", fmt.Errorf("tavily returned HTTP %d", resp.StatusCode)
	}

	var tavilyResp struct {
		Answer  string `json:"answer"`
		Results []struct {
			Title   string  `json:"title"`
			URL     string  `json:"url"`
			Content string  `json:"content"`
			Score   float64 `json:"score"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &tavilyResp); err != nil {
		return "", fmt.Errorf("parse tavily response: %w", err)
	}

	var sb strings.Builder
	if tavilyResp.Answer != "" {
		sb.WriteString("Answer: ")
		sb.WriteString(tavilyResp.Answer)
		sb.WriteString("\n\n")
	}
	sb.WriteString("Search Results:\n")
	for i, r := range tavilyResp.Results {
		fmt.Fprintf(&sb, "\n%d. %s\n   URL: %s\n   %s\n", i+1, r.Title, r.URL, r.Content)
	}

	slog.Debug("tavily search completed",
		"query", query,
		"results", len(tavilyResp.Results),
		"duration", time.Since(start))

	return sb.String(), nil
}

// --- Shared message-building ---

// appendMessagesToSlice normalizes chatReq["messages"] (which may be []any or
// []map[string]any) into a []any and appends additional messages.
func appendMessagesToSlice(existing any, additional ...any) []any {
	var result []any
	switch m := existing.(type) {
	case []any:
		result = make([]any, len(m), len(m)+len(additional))
		copy(result, m)
	case []map[string]any:
		result = make([]any, 0, len(m)+len(additional))
		for _, msg := range m {
			result = append(result, msg)
		}
	}
	return append(result, additional...)
}

// executeSearchCalls runs Tavily for each web_search tool call and returns
// (toolResultMessages, hasClientToolCalls). Non-search tool calls are skipped
// but flagged via hasClientToolCalls so the caller can decide what to do.
func (p *Pipeline) executeSearchCalls(ctx context.Context, searchKey string,
	toolCalls []api.ChatChoiceToolCall) (toolResults []any, hasClientTools bool) {

	for _, tc := range toolCalls {
		if tc.Function.Name != webSearchFunctionName {
			hasClientTools = true
			// Add a synthetic tool result so the message array is valid
			// (Chat Completions requires a result for every tool call).
			// This allows re-sending to the backend with search context
			// even when client-side tools are present.
			toolResults = append(toolResults, map[string]any{
				"role":         "tool",
				"tool_call_id": tc.ID,
				"content":      "[This tool call has not been executed yet. Re-request it if you still need the result.]",
			})
			continue
		}

		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			slog.Warn("failed to parse web_search arguments", "error", err, "raw", tc.Function.Arguments)
			args.Query = tc.Function.Arguments // best-effort fallback
		}

		result, err := p.ExecuteTavilySearch(ctx, searchKey, args.Query)
		if err != nil {
			slog.Warn("tavily search failed", "query", args.Query, "error", err)
			result = fmt.Sprintf("Web search failed: %s", err.Error())
		}

		toolResults = append(toolResults, map[string]any{
			"role":         "tool",
			"tool_call_id": tc.ID,
			"content":      result,
		})
	}
	return
}

// buildSearchContinuation constructs a new chatReq with the assistant's tool calls
// and search results appended to the message history. Returns a shallow copy of
// chatReq with updated messages (does not mutate the original).
func (p *Pipeline) buildSearchContinuation(ctx context.Context, chatReq map[string]any,
	searchKey string, toolCalls []api.ChatChoiceToolCall, assistantContent string) (map[string]any, bool, error) {

	assistantMsg := map[string]any{
		"role":       "assistant",
		"tool_calls": marshalToolCalls(toolCalls),
	}
	if assistantContent != "" {
		assistantMsg["content"] = assistantContent
	}

	newMessages := appendMessagesToSlice(chatReq["messages"], assistantMsg)
	toolResults, hasClientTools := p.executeSearchCalls(ctx, searchKey, toolCalls)
	newMessages = append(newMessages, toolResults...)

	newReq := make(map[string]any, len(chatReq))
	for k, v := range chatReq {
		newReq[k] = v
	}
	newReq["messages"] = newMessages
	return newReq, hasClientTools, nil
}

// --- Non-streaming search loop ---

// HandleNonStreamingSearchLoop handles the non-streaming tool loop for web search.
// It takes the already-parsed chatResponse from the first backend call. If the
// response contains web_search tool calls, it executes them and re-sends.
// Returns the final response (after all search iterations).
func (p *Pipeline) HandleNonStreamingSearchLoop(ctx context.Context, chatReq map[string]any,
	model *config.ModelConfig, firstResp *api.ChatResponse,
	sendRequest func(map[string]any) (*api.ChatResponse, error),
	maxIterations int) (*api.ChatResponse, error) {

	searchKey := p.ResolveWebSearchKey(model)
	if searchKey == "" {
		return firstResp, nil
	}

	resp := firstResp
	for i := 0; i < maxIterations; i++ {
		if len(resp.Choices) == 0 || !HasSearchToolCall(resp.Choices[0].Message.ToolCalls) {
			return resp, nil
		}

		choice := resp.Choices[0]
		content := ""
		if choice.Message.Content != nil {
			content = *choice.Message.Content
		}

		newReq, hasClientTools, err := p.buildSearchContinuation(
			ctx, chatReq, searchKey, choice.Message.ToolCalls, content)
		if err != nil {
			return nil, fmt.Errorf("search continuation: %w", err)
		}

		if hasClientTools {
			// Mixed tool calls: the backend requested both web_search and client
			// tools (bash, etc.) in one response. We've executed the search and
			// added synthetic "pending" results for client tools. Re-send to the
			// backend so it can make a new decision with search context. It will
			// typically re-request the client tools or produce a final answer.
			slog.Debug("search loop: mixed tool calls, re-sending with search context")
		}

		chatReq = newReq
		slog.Debug("search loop iteration", "iteration", i+1)

		resp, err = sendRequest(chatReq)
		if err != nil {
			return nil, err
		}
	}

	return resp, nil
}

// --- Streaming search support ---

// ExecuteSearchAndResend builds a new chatReq with search results appended.
// Used by streaming handlers after detecting web_search tool calls at finish_reason.
func (p *Pipeline) ExecuteSearchAndResend(ctx context.Context, chatReq map[string]any,
	model *config.ModelConfig, toolCalls []api.ChatChoiceToolCall, assistantContent string) (map[string]any, error) {

	searchKey := p.ResolveWebSearchKey(model)
	if searchKey == "" {
		return nil, fmt.Errorf("no search key configured")
	}

	newReq, _, err := p.buildSearchContinuation(ctx, chatReq, searchKey, toolCalls, assistantContent)
	return newReq, err
}

// streamingSearchState tracks accumulated tool calls during streaming to detect
// web_search calls that need proxy-side execution.
type streamingSearchState struct {
	ToolCalls []streamingToolCall
}

type streamingToolCall struct {
	ID   string
	Name string
	Args strings.Builder
}

// accumulateToolCall records a new tool call from a streaming chunk.
func (s *streamingSearchState) accumulateToolCall(id, name string) int {
	idx := len(s.ToolCalls)
	s.ToolCalls = append(s.ToolCalls, streamingToolCall{ID: id, Name: name})
	return idx
}

// appendArgs appends arguments to a tracked tool call.
func (s *streamingSearchState) appendArgs(idx int, args string) {
	if idx >= 0 && idx < len(s.ToolCalls) {
		s.ToolCalls[idx].Args.WriteString(args)
	}
}

// hasSearchCall returns true if any accumulated tool call is web_search.
func (s *streamingSearchState) hasSearchCall() bool {
	for _, tc := range s.ToolCalls {
		if tc.Name == webSearchFunctionName {
			return true
		}
	}
	return false
}

// onlySearchCalls returns true if ALL accumulated tool calls are web_search.
func (s *streamingSearchState) onlySearchCalls() bool {
	if len(s.ToolCalls) == 0 {
		return false
	}
	for _, tc := range s.ToolCalls {
		if tc.Name != webSearchFunctionName {
			return false
		}
	}
	return true
}

// toChatChoiceToolCalls converts accumulated streaming state to ChatChoiceToolCall format.
func (s *streamingSearchState) toChatChoiceToolCalls() []api.ChatChoiceToolCall {
	result := make([]api.ChatChoiceToolCall, len(s.ToolCalls))
	for i, tc := range s.ToolCalls {
		result[i] = api.ChatChoiceToolCall{
			ID:   tc.ID,
			Type: "function",
			Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{
				Name:      tc.Name,
				Arguments: tc.Args.String(),
			},
		}
	}
	return result
}

// --- Helpers ---

// marshalToolCalls converts typed tool calls back to map format for JSON serialization.
func marshalToolCalls(tcs []api.ChatChoiceToolCall) []any {
	result := make([]any, len(tcs))
	for i, tc := range tcs {
		result[i] = map[string]any{
			"id":   tc.ID,
			"type": tc.Type,
			"function": map[string]any{
				"name":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
			},
		}
	}
	return result
}
