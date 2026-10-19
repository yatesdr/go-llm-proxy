// responses_translate.go contains the translation logic for converting
// Responses API requests into Chat Completions format.
package handler

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strings"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/pipeline"
)

// --- Request types ---

type responsesRequest struct {
	Model              string            `json:"model"`
	Input              json.RawMessage   `json:"input"`
	Instructions       string            `json:"instructions,omitempty"`
	Tools              []json.RawMessage `json:"tools,omitempty"`
	ToolChoice         json.RawMessage   `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool             `json:"parallel_tool_calls,omitempty"`
	Temperature        *float64          `json:"temperature,omitempty"`
	TopP               *float64          `json:"top_p,omitempty"`
	MaxOutputTokens    *int              `json:"max_output_tokens,omitempty"`
	Stream             bool              `json:"stream"`
	Reasoning          *reasoningConfig  `json:"reasoning,omitempty"`
	Text               json.RawMessage   `json:"text,omitempty"`
	User               string            `json:"user,omitempty"`
	Store              *bool             `json:"store,omitempty"`
	PreviousResponseID string            `json:"previous_response_id,omitempty"`
	PromptCacheKey     string            `json:"prompt_cache_key,omitempty"`
	aliasFrom          string

	// Set by the translating handler once the Chat Completions request is
	// built: the tool-name registry used to map tool calls back to Responses
	// items, and whether the client asked for structured (JSON) output.
	tools    *toolRegistry
	jsonMode bool
}

type reasoningConfig struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// inputItem represents an item in the Responses API input array.
type inputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	ID        string          `json:"id"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Arguments string          `json:"arguments"` // function_call
	Input     string          `json:"input"`     // custom_tool_call
	Output    json.RawMessage `json:"output"`    // *_output items (string, array, or object)
	Action    json.RawMessage `json:"action"`    // local_shell_call
	Status    string          `json:"status"`
	Summary   json.RawMessage `json:"summary"` // reasoning
	Tools     json.RawMessage `json:"tools"`   // additional_tools
}

// --- Input translation (Responses API -> Chat Completions) ---

// translateOptions tunes translateInputWith.
type translateOptions struct {
	// DeveloperRole is how developer/system messages that are NOT part of the
	// leading run are sent: config.DeveloperRoleUser (default),
	// DeveloperRoleSystem or DeveloperRoleDeveloper.
	DeveloperRole string
	// SystemPrefix is text placed first in the merged system message
	// (compaction uses it for its summarization prompt).
	SystemPrefix string
	// Tools resolves (namespace, name) to the flattened chat function name.
	// May be nil.
	Tools *toolRegistry
}

// leadingSystemSeparator joins the parts of the merged leading system
// message: `instructions` and each leading system/developer message (and each
// text part inside one message) are separated by exactly one blank line.
const leadingSystemSeparator = "\n\n"

// developerOpen and developerClose wrap later developer messages when
// developer_role is "user" (the default).
const (
	developerOpen  = "<developer>\n"
	developerClose = "\n</developer>"
)

// translateInput converts Responses API input items into Chat Completions
// messages using default options. Kept for callers (and tests) that do not
// need tool resolution.
func translateInput(input json.RawMessage, instructions string) ([]map[string]any, error) {
	return translateInputWith(input, instructions, translateOptions{})
}

// translateInputWith converts Responses API input items into Chat Completions
// messages.
//
// System prompt handling: `instructions` (if any) and the LEADING contiguous
// run of system/developer messages are merged into ONE system message, in
// order, joined with a blank line. Strict chat templates (Qwen, Gemma, ...)
// reject a system message anywhere but first, and one stable first message
// keeps prefix caches warm. Items that produce no message (additional_tools,
// reasoning, ...) do not end the run. Any system/developer message after the
// first non-system message is sent per opts.DeveloperRole.
func translateInputWith(input json.RawMessage, instructions string, opts translateOptions) ([]map[string]any, error) {
	var messages []map[string]any
	var sysParts []string
	if opts.SystemPrefix != "" {
		sysParts = append(sysParts, opts.SystemPrefix)
	}
	if instructions != "" {
		sysParts = append(sysParts, instructions)
	}

	finish := func() ([]map[string]any, error) {
		if len(messages) == 0 && len(sysParts) == 0 {
			return nil, fmt.Errorf("no valid input items")
		}
		if len(sysParts) > 0 {
			sys := map[string]any{
				"role":    "system",
				"content": strings.Join(sysParts, leadingSystemSeparator),
			}
			messages = append([]map[string]any{sys}, messages...)
		}
		return messages, nil
	}

	// String input: wrap as a single user message.
	var inputStr string
	if json.Unmarshal(input, &inputStr) == nil {
		messages = append(messages, map[string]any{
			"role":    "user",
			"content": inputStr,
		})
		return finish()
	}

	// Array input.
	var items []json.RawMessage
	if err := json.Unmarshal(input, &items); err != nil {
		return nil, fmt.Errorf("input must be a string or array")
	}

	// Reasoning text waiting to be attached (as reasoning_content) to the next
	// assistant message. Anything else in between drops it.
	pendingReasoning := ""
	attachReasoning := func(msg map[string]any) {
		if pendingReasoning == "" {
			return
		}
		if prev, _ := msg["reasoning_content"].(string); prev != "" {
			msg["reasoning_content"] = prev + "\n\n" + pendingReasoning
		} else {
			msg["reasoning_content"] = pendingReasoning
		}
		pendingReasoning = ""
	}

	for _, raw := range items {
		var item inputItem
		if json.Unmarshal(raw, &item) != nil {
			continue
		}

		switch {
		case item.Type == "function_call" || item.Type == "local_shell_call" || item.Type == "custom_tool_call":
			args := item.Arguments
			if item.Type == "custom_tool_call" {
				// Custom (freeform) tools are presented to the model as a
				// function with one string parameter named "input".
				b, _ := json.Marshal(map[string]string{"input": item.Input})
				args = string(b)
			} else {
				if args == "" && item.Input != "" {
					args = item.Input
				}
				if args == "" && len(item.Action) > 0 && string(item.Action) != "null" {
					args = string(item.Action)
				}
				if args == "" {
					args = "{}"
				}
			}
			name := item.Name
			if name == "" && item.Type == "local_shell_call" {
				name = "shell"
			}
			name = opts.Tools.flatName(item.Namespace, name)

			tc := map[string]any{
				"id":   item.CallID,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": args,
				},
			}

			// Merge into the last message if it's an assistant message.
			merged := false
			if n := len(messages); n > 0 {
				last := messages[n-1]
				if last["role"] == "assistant" {
					existing, _ := last["tool_calls"].([]any)
					last["tool_calls"] = append(existing, tc)
					attachReasoning(last)
					merged = true
				}
			}
			if !merged {
				msg := map[string]any{
					"role":       "assistant",
					"content":    nil,
					"tool_calls": []any{tc},
				}
				attachReasoning(msg)
				messages = append(messages, msg)
			}

		case item.Type == "function_call_output" || item.Type == "local_shell_call_output" ||
			item.Type == "custom_tool_call_output" || item.Type == "mcp_tool_call_output":
			pendingReasoning = ""
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": item.CallID,
				"content":      translateToolOutput(item.Output),
			})

		case item.Type == "reasoning":
			if t := reasoningItemText(item); t != "" {
				if pendingReasoning != "" {
					pendingReasoning += "\n\n"
				}
				pendingReasoning += t
			}

		case item.Type == "additional_tools" || item.Type == "compaction" ||
			item.Type == "tool_search_call" || item.Type == "tool_search_output" ||
			item.Type == "web_search_call" || item.Type == "image_generation_call" ||
			item.Type == "mcp_list_tools":
			continue // tool definitions are collected separately; the rest has no Chat Completions equivalent

		case item.Role == "system" || item.Role == "developer":
			text := messageText(item.Content)
			if text == "" {
				continue
			}
			if len(messages) == 0 {
				sysParts = append(sysParts, text)
				continue
			}
			pendingReasoning = ""
			messages = append(messages, laterDeveloperMessage(text, opts.DeveloperRole))

		case item.Role != "":
			content := translateContentForChat(item.Content, item.Role)
			msg := map[string]any{
				"role":    item.Role,
				"content": content,
			}
			if item.Role == "assistant" {
				attachReasoning(msg)
			} else {
				pendingReasoning = ""
			}
			messages = append(messages, msg)

		default:
			continue
		}
	}

	return finish()
}

// laterDeveloperMessage builds the chat message for a system/developer
// message that follows the conversation's first turn.
//
//	developer_role: user      -> role "user", content "<developer>\n{text}\n</developer>"
//	developer_role: system    -> role "system", content {text}
//	developer_role: developer -> role "developer", content {text}
func laterDeveloperMessage(text, developerRole string) map[string]any {
	switch developerRole {
	case config.DeveloperRoleSystem:
		return map[string]any{"role": "system", "content": text}
	case config.DeveloperRoleDeveloper:
		return map[string]any{"role": "developer", "content": text}
	}
	return map[string]any{"role": "user", "content": developerOpen + text + developerClose}
}

// messageText flattens message content to plain text. A string passes through;
// for part arrays the text parts are joined with a blank line. Non-text parts
// are ignored.
func messageText(content json.RawMessage) string {
	if len(content) == 0 || string(content) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &parts) != nil {
		return ""
	}
	var texts []string
	for _, p := range parts {
		if p.Text != "" && (p.Type == "input_text" || p.Type == "output_text" || p.Type == "text") {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, leadingSystemSeparator)
}

// reasoningItemText returns the raw reasoning text of a replayed reasoning
// item: its content (reasoning_text) parts, or the summary when the item has
// no content. This proxy emits both with the same text, so only one is used.
func reasoningItemText(item inputItem) string {
	collect := func(raw json.RawMessage) string {
		var parts []struct {
			Text string `json:"text"`
		}
		if len(raw) == 0 || json.Unmarshal(raw, &parts) != nil {
			return ""
		}
		var texts []string
		for _, p := range parts {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, "\n\n")
	}
	if t := collect(item.Content); t != "" {
		return t
	}
	return collect(item.Summary)
}

// collectAdditionalTools returns the tool definitions carried by
// `additional_tools` input items (Codex's auto-review sends its tools this
// way instead of in the top-level tools array).
func collectAdditionalTools(input json.RawMessage) []json.RawMessage {
	var items []json.RawMessage
	if json.Unmarshal(input, &items) != nil {
		return nil
	}
	var out []json.RawMessage
	for _, raw := range items {
		var item struct {
			Type  string            `json:"type"`
			Tools []json.RawMessage `json:"tools"`
		}
		if json.Unmarshal(raw, &item) != nil || item.Type != "additional_tools" {
			continue
		}
		out = append(out, item.Tools...)
	}
	return out
}

// translateContentForChat converts Responses API content to Chat Completions format.
func translateContentForChat(content json.RawMessage, role string) any {
	if len(content) == 0 || string(content) == "null" {
		return ""
	}

	// String content: pass through.
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}

	// Array of content parts.
	var parts []map[string]json.RawMessage
	if json.Unmarshal(content, &parts) != nil {
		return string(content)
	}

	// For assistant messages, extract text from output_text parts.
	if role == "assistant" {
		var texts []string
		for _, p := range parts {
			var partType string
			json.Unmarshal(p["type"], &partType)
			if partType == "output_text" {
				var text string
				json.Unmarshal(p["text"], &text)
				texts = append(texts, text)
			}
		}
		return strings.Join(texts, "")
	}

	// For user messages, translate input part types.
	var translated []map[string]any
	for _, p := range parts {
		var partType string
		json.Unmarshal(p["type"], &partType)

		switch partType {
		case "input_text":
			var text string
			json.Unmarshal(p["text"], &text)
			translated = append(translated, map[string]any{
				"type": "text",
				"text": text,
			})
		case "input_image":
			translated = append(translated, translateInputImage(p))
		case "input_file":
			translated = append(translated, translateInputFile(p))
		default:
			var part map[string]any
			raw, _ := json.Marshal(p)
			json.Unmarshal(raw, &part)
			translated = append(translated, part)
		}
	}
	if len(translated) > 0 {
		return translated
	}
	return ""
}

// translateInputImage converts an input_image part. A PDF masquerading as an
// image (Codex and other clients sometimes submit PDFs this way, with a
// data:application/pdf URL) becomes the pipeline-internal pdf_data part so it
// gets the full text/OCR/vision cascade instead of the image pipeline.
func translateInputImage(p map[string]json.RawMessage) map[string]any {
	var url string
	json.Unmarshal(p["image_url"], &url)
	if data, ok := pipeline.DecodePDFDataURL(url); ok {
		var fn string
		json.Unmarshal(p["filename"], &fn)
		return pdfDataPart(data, fn)
	}
	part := map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": url},
	}
	if detail, ok := p["detail"]; ok {
		var d string
		json.Unmarshal(detail, &d)
		if d != "" {
			part["image_url"].(map[string]any)["detail"] = d
		}
	}
	return part
}

// translateInputFile converts a Responses input_file part. Inline PDFs
// (file_data as a data URL or bare base64) become the pipeline-internal
// pdf_data part; other inline files become a Chat Completions `file` part.
// Files referenced by id or URL cannot be read here and become a short text
// note so the model knows something was attached.
func translateInputFile(p map[string]json.RawMessage) map[string]any {
	var fileData, filename string
	json.Unmarshal(p["file_data"], &fileData)
	json.Unmarshal(p["filename"], &filename)

	if fileData == "" {
		label := filename
		if label == "" {
			label = "unnamed file"
		}
		return map[string]any{
			"type": "text",
			"text": fmt.Sprintf("[attached file %q could not be read: only inline file_data is supported]", label),
		}
	}

	if data, ok := pipeline.DecodePDFDataURL(fileData); ok {
		return pdfDataPart(data, filename)
	}
	if !strings.HasPrefix(fileData, "data:") {
		// Bare base64: a PDF if the name says so or the payload starts with
		// "%PDF-" (base64 "JVBERi").
		if strings.HasSuffix(strings.ToLower(filename), ".pdf") || strings.HasPrefix(fileData, "JVBERi") {
			return pdfDataPart(fileData, filename)
		}
	}
	file := map[string]any{"file_data": fileData}
	if filename != "" {
		file["filename"] = filename
	}
	return map[string]any{"type": "file", "file": file}
}

func pdfDataPart(b64, filename string) map[string]any {
	part := map[string]any{"type": "pdf_data", "data": b64}
	if filename != "" {
		part["filename"] = filename
	}
	return part
}

// downgradePDFData rewrites any pdf_data part still present in the request
// (the pipeline did not run or left it alone) into the OpenAI Chat Completions
// `file` part, so a backend with native PDF support still receives it and the
// internal marker never leaks upstream.
func downgradePDFData(chatReq map[string]any) {
	var messages []map[string]any
	switch m := chatReq["messages"].(type) {
	case []map[string]any:
		messages = m
	case []any:
		for _, x := range m {
			if mm, ok := x.(map[string]any); ok {
				messages = append(messages, mm)
			}
		}
	default:
		return
	}
	convert := func(part map[string]any) map[string]any {
		if part["type"] != "pdf_data" {
			return part
		}
		data, _ := part["data"].(string)
		fn, _ := part["filename"].(string)
		if fn == "" {
			fn = "document.pdf"
		}
		return map[string]any{"type": "file", "file": map[string]any{
			"filename":  fn,
			"file_data": "data:application/pdf;base64," + data,
		}}
	}
	for _, msg := range messages {
		switch c := msg["content"].(type) {
		case []map[string]any:
			for i, part := range c {
				c[i] = convert(part)
			}
		case []any:
			for i, p := range c {
				if part, ok := p.(map[string]any); ok {
					c[i] = convert(part)
				}
			}
		}
	}
}

// --- Tool output translation ---

// translateToolOutput converts a Responses API tool output value into Chat Completions
// content. Handles three formats:
//   - string: passed through as-is (common case)
//   - array: content items like [{"type":"input_image","image_url":"data:..."}] — translated
//     to Chat Completions multimodal content parts
//   - object: structured output like {"content":"text","success":true} or
//     {"content":[...],"success":true} — content field extracted and translated
func translateToolOutput(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}

	// Try string first (most common).
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}

	// Try array of content items.
	var arr []map[string]json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		return translateOutputContentItems(arr)
	}

	// Try object with content + success fields.
	var obj struct {
		Content json.RawMessage `json:"content"`
		Success *bool           `json:"success"`
	}
	if json.Unmarshal(raw, &obj) == nil && len(obj.Content) > 0 {
		// Content can be a string or an array.
		var contentStr string
		if json.Unmarshal(obj.Content, &contentStr) == nil {
			return contentStr
		}
		var contentArr []map[string]json.RawMessage
		if json.Unmarshal(obj.Content, &contentArr) == nil {
			return translateOutputContentItems(contentArr)
		}
	}

	// Fallback: pass raw JSON as string.
	return string(raw)
}

// translateOutputContentItems converts Responses API content items (input_text, input_image)
// to Chat Completions format (text, image_url).
func translateOutputContentItems(items []map[string]json.RawMessage) []map[string]any {
	var parts []map[string]any
	for _, item := range items {
		var partType string
		json.Unmarshal(item["type"], &partType)

		switch partType {
		case "input_text":
			var text string
			json.Unmarshal(item["text"], &text)
			parts = append(parts, map[string]any{
				"type": "text",
				"text": text,
			})
		case "input_image":
			parts = append(parts, translateInputImage(item))
		case "input_file":
			parts = append(parts, translateInputFile(item))
		default:
			// Pass through unknown types as-is.
			var part map[string]any
			raw, _ := json.Marshal(item)
			json.Unmarshal(raw, &part)
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return parts
}

// --- Tool translation ---

// toolBinding records what a flattened chat function name stands for.
type toolBinding struct {
	Namespace string // "" for a top-level tool
	Name      string // the tool's own name inside its namespace
	Custom    bool   // freeform tool (input string) rather than a JSON-arguments function
}

// toolRegistry maps between Responses tools and the chat functions presented
// to the model, in both directions. A nil registry is valid and resolves
// nothing.
//
// Flattening rule (deterministic, collision-safe):
//   - a top-level tool keeps its name;
//   - a tool inside namespace NS is exposed as "NS__NAME" with characters
//     outside [A-Za-z0-9_-] replaced by "_", cut to 64 characters (the tail
//     replaced by "_" + an 8-hex hash of the full name when too long);
//   - the namespace "functions" (the implicit default namespace) is dropped
//     when the bare name is free, so Codex's auto-review sees `exec` rather
//     than `functions__exec`;
//   - if the candidate is already taken (a top-level tool of that name always
//     wins) the full "NS__NAME" form is tried, then "_2", "_3", ... suffixes.
type toolRegistry struct {
	byFlat map[string]toolBinding
	byKey  map[string]string // namespace+"\x00"+name -> flat name
	byName map[string]string // bare name -> flat name (first definition)
}

func registryKey(ns, name string) string { return ns + "\x00" + name }

// flatName returns the chat function name for a Responses (namespace, name)
// pair. Unknown tools (history mentioning a tool not defined in this request)
// get the name the flattening rule would give them.
func (r *toolRegistry) flatName(ns, name string) string {
	if r != nil {
		if flat, ok := r.byKey[registryKey(ns, name)]; ok {
			return flat
		}
		if ns == "" {
			if flat, ok := r.byName[name]; ok {
				return flat
			}
		}
	}
	if ns == "" {
		return name
	}
	return sanitizeToolName(ns + "__" + name)
}

// resolve maps a chat function name back to its Responses tool.
func (r *toolRegistry) resolve(flat string) (toolBinding, bool) {
	if r == nil {
		return toolBinding{}, false
	}
	b, ok := r.byFlat[flat]
	return b, ok
}

func sanitizeToolName(s string) string {
	var sb strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
			sb.WriteRune(c)
		default:
			sb.WriteByte('_')
		}
	}
	out := sb.String()
	if len(out) > 64 {
		h := fnv.New32a()
		h.Write([]byte(s))
		out = fmt.Sprintf("%s_%08x", out[:55], h.Sum32())
	}
	return out
}

// translatedTools is the result of translateTools.
type translatedTools struct {
	Tools    []map[string]any // Chat Completions tool definitions
	Stripped []string         // server-side tool types with no chat equivalent (web_search, ...)
	Registry *toolRegistry
}

// customToolParameters is the schema every freeform tool is given.
var customToolParameters = json.RawMessage(`{"type":"object","properties":{"input":{"type":"string","description":"The raw tool input, passed verbatim."}},"required":["input"],"additionalProperties":false}`)

// translateTools converts Responses API tool definitions to Chat Completions
// tools:
//   - function tools map one to one;
//   - namespace tools are flattened (see toolRegistry);
//   - custom (freeform) tools become a function with a single string
//     parameter `input`; a grammar definition is appended to the description;
//   - any other type (web_search, ...) is stripped and listed in Stripped.
func translateTools(tools []json.RawMessage) translatedTools {
	type def struct {
		ns, name, desc string
		params         json.RawMessage
		strict         *bool
		custom         bool
		format         json.RawMessage
	}
	var defs []def
	var stripped []string

	type rawTool struct {
		Type        string            `json:"type"`
		Name        string            `json:"name"`
		Description string            `json:"description"`
		Parameters  json.RawMessage   `json:"parameters"`
		Strict      *bool             `json:"strict"`
		Format      json.RawMessage   `json:"format"`
		Tools       []json.RawMessage `json:"tools"`
	}
	add := func(ns string, t rawTool) bool {
		switch t.Type {
		case "function":
			defs = append(defs, def{ns: ns, name: t.Name, desc: t.Description, params: t.Parameters, strict: t.Strict})
		case "custom":
			defs = append(defs, def{ns: ns, name: t.Name, desc: t.Description, custom: true, format: t.Format})
		default:
			return false
		}
		return true
	}
	for _, raw := range tools {
		var t rawTool
		if json.Unmarshal(raw, &t) != nil {
			continue
		}
		if t.Type == "namespace" {
			for _, sub := range t.Tools {
				var st rawTool
				if json.Unmarshal(sub, &st) != nil {
					continue
				}
				if !add(t.Name, st) {
					slog.Debug("stripping unsupported tool from namespace", "namespace", t.Name, "type", st.Type)
				}
			}
			continue
		}
		if !add("", t) {
			slog.Debug("stripping non-function tool from translated request", "type", t.Type)
			stripped = append(stripped, t.Type)
		}
	}

	reg := &toolRegistry{
		byFlat: map[string]toolBinding{},
		byKey:  map[string]string{},
		byName: map[string]string{},
	}
	// Top-level names are reserved first so they always keep their own name.
	used := map[string]bool{}
	for _, d := range defs {
		if d.ns == "" {
			used[d.name] = true
		}
	}

	var out []map[string]any
	for _, d := range defs {
		key := registryKey(d.ns, d.name)
		if _, dup := reg.byKey[key]; dup {
			continue
		}
		flat := d.name
		if d.ns != "" {
			flat = sanitizeToolName(d.ns + "__" + d.name)
			if d.ns == "functions" {
				if bare := sanitizeToolName(d.name); !used[bare] {
					flat = bare
				}
			}
			if used[flat] {
				base := flat
				for i := 2; used[flat]; i++ {
					flat = sanitizeToolName(fmt.Sprintf("%s_%d", base, i))
				}
			}
			used[flat] = true
		}
		reg.byKey[key] = flat
		reg.byFlat[flat] = toolBinding{Namespace: d.ns, Name: d.name, Custom: d.custom}
		if _, ok := reg.byName[d.name]; !ok {
			reg.byName[d.name] = flat
		}

		fn := map[string]any{"name": flat}
		if d.custom {
			fn["description"] = customToolDescription(d.desc, d.format)
			fn["parameters"] = customToolParameters
		} else {
			if d.desc != "" {
				fn["description"] = d.desc
			}
			if len(d.params) > 0 {
				fn["parameters"] = d.params
			}
			if d.strict != nil {
				fn["strict"] = *d.strict
			}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return translatedTools{Tools: out, Stripped: stripped, Registry: reg}
}

// customToolDescription explains the freeform contract to the model and, for
// grammar-constrained tools, appends the grammar (the backend cannot enforce
// it, but the model can follow it).
func customToolDescription(desc string, format json.RawMessage) string {
	note := "This tool takes freeform text: pass it verbatim as the `input` string argument."
	var f struct {
		Type       string `json:"type"`
		Syntax     string `json:"syntax"`
		Definition string `json:"definition"`
	}
	if len(format) > 0 && json.Unmarshal(format, &f) == nil && f.Type == "grammar" && strings.TrimSpace(f.Definition) != "" {
		def := strings.TrimSpace(f.Definition)
		if len(def) > 4000 {
			def = def[:4000]
		}
		note += fmt.Sprintf("\nThe input must match this %s grammar:\n%s", f.Syntax, def)
	}
	if desc == "" {
		return note
	}
	return desc + "\n\n" + note
}

// translateToolChoice converts a Responses tool_choice. Strings pass through
// ("auto", "none", "required"); {type: function|custom, name} becomes the
// chat form with the flattened name; allowed_tools collapses to its mode;
// anything else becomes "auto".
func translateToolChoice(raw json.RawMessage, reg *toolRegistry) any {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Mode      string `json:"mode"`
		Function  *struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return "auto"
	}
	switch obj.Type {
	case "function", "custom":
		name := obj.Name
		if name == "" && obj.Function != nil {
			name = obj.Function.Name
		}
		if name == "" {
			return "auto"
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": reg.flatName(obj.Namespace, name)}}
	case "allowed_tools":
		if obj.Mode == "required" {
			return "required"
		}
	}
	return "auto"
}

// --- Text format translation ---

// translateTextFormat converts Responses API text.format to Chat Completions
// response_format. text.verbosity has no chat equivalent and is dropped.
func translateTextFormat(text json.RawMessage) json.RawMessage {
	if len(text) == 0 {
		return nil
	}
	var tf struct {
		Format struct {
			Type   string          `json:"type"`
			Name   string          `json:"name,omitempty"`
			Schema json.RawMessage `json:"schema,omitempty"`
			Strict *bool           `json:"strict,omitempty"`
		} `json:"format"`
	}
	if json.Unmarshal(text, &tf) != nil {
		return nil
	}
	switch tf.Format.Type {
	case "json_schema":
		name := tf.Format.Name
		if name == "" {
			name = "response"
		}
		schema := map[string]any{"name": name}
		if len(tf.Format.Schema) > 0 {
			schema["schema"] = json.RawMessage(tf.Format.Schema)
		}
		if tf.Format.Strict != nil {
			schema["strict"] = *tf.Format.Strict
		}
		result, _ := json.Marshal(map[string]any{
			"type":        "json_schema",
			"json_schema": schema,
		})
		return result
	case "json_object":
		result, _ := json.Marshal(map[string]any{"type": "json_object"})
		return result
	}
	return nil // "text" format needs no response_format
}

// --- Chat Completions request builder ---

// prepareChatRequest translates a whole Responses request: messages (system
// prompt merged per translateInputWith), tools (including those carried by
// additional_tools input items), and sampling/format fields. It sets req.tools
// and req.jsonMode for the response translators.
func prepareChatRequest(req *responsesRequest, model *config.ModelConfig) (map[string]any, error) {
	allTools := append(append([]json.RawMessage(nil), req.Tools...), collectAdditionalTools(req.Input)...)
	tt := translateTools(allTools)
	req.tools = tt.Registry

	messages, err := translateInputWith(req.Input, req.Instructions, translateOptions{
		DeveloperRole: model.DeveloperRole,
		Tools:         tt.Registry,
	})
	if err != nil {
		return nil, err
	}
	chatReq := buildChatRequest(*req, model.Model, messages, tt)
	_, req.jsonMode = chatReq["response_format"]
	return chatReq, nil
}

func buildChatRequest(req responsesRequest, backendModel string, messages []map[string]any, tt translatedTools) map[string]any {
	chatReq := map[string]any{
		"model":    backendModel,
		"messages": messages,
		"stream":   req.Stream,
	}
	if req.Stream {
		chatReq["stream_options"] = map[string]any{"include_usage": true}
	}
	if len(tt.Tools) > 0 {
		chatReq["tools"] = tt.Tools
		// Only include tool_choice and parallel_tool_calls when tools are present.
		if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
			chatReq["tool_choice"] = translateToolChoice(req.ToolChoice, tt.Registry)
		}
		if req.ParallelToolCalls != nil {
			chatReq["parallel_tool_calls"] = *req.ParallelToolCalls
		}
	}
	if len(tt.Stripped) > 0 {
		chatReq[pipeline.InternalKeyStrippedTools] = tt.Stripped
	}
	if req.Temperature != nil {
		chatReq["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		chatReq["top_p"] = *req.TopP
	}
	if req.MaxOutputTokens != nil {
		chatReq["max_completion_tokens"] = *req.MaxOutputTokens
	}
	// Effort is backend-specific (low/medium/high/xhigh/...): pass it through
	// untouched and let the backend judge. reasoning.summary and
	// reasoning.context have no chat equivalent.
	if req.Reasoning != nil && strings.TrimSpace(req.Reasoning.Effort) != "" {
		chatReq["reasoning_effort"] = strings.TrimSpace(req.Reasoning.Effort)
	}
	if len(req.Text) > 0 {
		if rf := translateTextFormat(req.Text); rf != nil {
			chatReq["response_format"] = json.RawMessage(rf)
		}
	}
	if req.User != "" {
		chatReq["user"] = req.User
	}
	return chatReq
}
