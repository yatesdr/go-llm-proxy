package handler

import (
	"log/slog"

	"go-llm-proxy/internal/config"
)

// applyModelOptions applies per-model behaviour to a translated request after
// it has been built from the Messages request:
//
//   - thinking.type "disabled" merges the model's thinking_off object (the
//     backend-specific way to switch reasoning off, e.g. chat_template_kwargs);
//   - the model's sampling defaults fill any field the client did not send.
func applyModelOptions(chatReq map[string]any, req messagesRequest, model *config.ModelConfig) {
	thinkingDisabled := parseThinking(req.Thinking).Type == "disabled"
	if thinkingDisabled && len(model.ThinkingOff) > 0 {
		mergeJSONObject(chatReq, model.ThinkingOff)
	}

	_, hadReasoning := chatReq["reasoning_effort"]
	_, hadMaxCompletion := chatReq["max_completion_tokens"]
	model.ApplySamplingDefaults(chatReq)
	// The translated request always uses max_completion_tokens; a configured
	// max_new_tokens default must not add a conflicting max_tokens beside it.
	if hadMaxCompletion {
		delete(chatReq, "max_tokens")
	}
	// A reasoning-effort default would undo an explicit "thinking off".
	if thinkingDisabled && len(model.ThinkingOff) > 0 && !hadReasoning {
		delete(chatReq, "reasoning_effort")
	}
}

// mergeJSONObject deep-merges src into dst; src wins on conflicts. Nested
// objects are merged key by key so a request's own chat_template_kwargs
// survive alongside the configured ones.
func mergeJSONObject(dst map[string]any, src map[string]any) {
	for k, v := range src {
		sv, srcIsMap := v.(map[string]any)
		if dv, ok := dst[k].(map[string]any); ok && srcIsMap {
			mergeJSONObject(dv, sv)
			continue
		}
		if srcIsMap {
			// Copy so the shared config map is never mutated by later merges.
			cp := map[string]any{}
			mergeJSONObject(cp, sv)
			dst[k] = cp
			continue
		}
		dst[k] = v
	}
}

// clampMaxTokens lowers max_completion_tokens so prompt + completion fit the
// model's context window, when the window is known. The prompt size is an
// estimate; when it leaves no sensible room the request is sent as-is and the
// backend's context-length error (rewritten for the client) takes over.
func clampMaxTokens(chatReq map[string]any, body []byte, model *config.ModelConfig) bool {
	ctxWindow := model.ContextWindow
	if ctxWindow <= 0 {
		return false
	}
	maxTok, ok := chatReq["max_completion_tokens"].(int)
	if !ok || maxTok <= 0 {
		return false
	}
	prompt := estimateRequestTokens(body)
	// 10% + 128 tokens of headroom for estimation error and template overhead.
	room := ctxWindow - prompt - prompt/10 - 128
	const minUseful = 256
	if room < minUseful || maxTok <= room {
		return false
	}
	slog.Debug("clamping max_tokens to context window",
		"model", model.Name, "requested", maxTok, "clamped", room,
		"context_window", ctxWindow, "estimated_prompt", prompt)
	chatReq["max_completion_tokens"] = room
	return true
}

// base64Runs returns the [start,end) offsets of runs of at least minLen
// base64-alphabet bytes in body: embedded images and PDFs. Counting them at
// four characters per token would overstate them wildly.
func base64Runs(body []byte, minLen int) [][2]int {
	var runs [][2]int
	start := -1
	for i := 0; i <= len(body); i++ {
		in := false
		if i < len(body) {
			c := body[i]
			in = c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
				c == '+' || c == '/' || c == '=' || c == '_' || c == '-'
		}
		switch {
		case in && start < 0:
			start = i
		case !in && start >= 0:
			if i-start >= minLen {
				runs = append(runs, [2]int{start, i})
			}
			start = -1
		}
	}
	return runs
}

// estimateRequestTokens estimates the prompt size of a JSON request body: about
// four characters per token for text, with embedded base64 media costed
// separately (a flat figure per image, bytes-based for PDFs). It is only an
// estimate, matching the client's own fallback heuristic.
func estimateRequestTokens(body []byte) int {
	const (
		imageTokens = 1600
		maxPDFToks  = 60000
	)
	media := base64Runs(body, 2048)
	textBytes := len(body)
	tokens := 0
	for _, m := range media {
		n := m[1] - m[0]
		textBytes -= n
		if len(body[m[0]:m[1]]) >= 6 && string(body[m[0]:m[0]+6]) == "JVBERi" {
			t := n * 3 / 4 / 150
			if t > maxPDFToks {
				t = maxPDFToks
			}
			tokens += t
		} else {
			tokens += imageTokens
		}
	}
	return tokens + textBytes/4
}
