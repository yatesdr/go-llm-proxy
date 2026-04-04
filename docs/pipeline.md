# Processing Pipeline

go-llm-proxy includes a content processing pipeline that transparently handles images, PDFs, and web search for backends that don't support them natively. The pipeline runs automatically when configured — no client-side changes needed.

## Configuration

```yaml
processors:
  vision: Qwen3-VL-8B           # vision-capable model for image description
  ocr: paddleOCR                # dedicated OCR model for text extraction (optional)
  web_search_key: tvly-...      # Tavily API key for web search (optional)
```

The `vision` model is required for image processing. The `ocr` model is optional — if not configured, the vision model handles OCR duties as a fallback. See [config-reference.md](config-reference.md) for per-model overrides and additional options.

## Image processing

When a client sends an image to a text-only backend, the proxy intercepts the image, sends it to a vision-capable model for description, and replaces the image with the text description before forwarding to the backend.

### How images are handled per role

| Image source | Processing | Model used | Prompt |
|---|---|---|---|
| **User-attached image** (user message) | Vision description | `vision` model | Describe the image accurately and objectively |
| **Tool output image** (tool message — Codex `view_image`, screenshots) | OCR text extraction | `ocr` model (or `vision` fallback) | `OCR:` (dedicated OCR) or verbose extraction prompt (vision fallback) |

User-attached photos receive only vision description — dedicated OCR models produce unreliable output on natural photographs. Text visible in photos is captured adequately by the vision model's description.

Tool output images (PDF page renders, screenshots, Codex `view_image` results) are routed to the dedicated OCR model for accurate text extraction.

### Processing details

- Images are processed **concurrently** (up to 5 in parallel)
- Results are **cached by content hash** — follow-up turns with the same image are instant
- Maximum **10 images per request** — additional images get a placeholder
- Cache keys include a mode suffix (`:v` for vision, `:o` for OCR) so results are stored independently
- Reasoning/thinking is disabled for vision model calls to maximize output quality

## PDF processing

PDF handling depends on the client and the PDF content type.

### Claude Code

Claude Code sends PDF content as base64 document blocks in the API request. The proxy handles them in two stages:

| Stage | Condition | Action |
|---|---|---|
| **Stage 1: Text extraction** | Always attempted | Pure Go text extraction — no external dependencies. Fast, accurate for native PDFs. |
| **Stage 2: OCR fallback** | Text extraction returns little/no content (scanned PDF) | Send PDF as image to OCR model (`ocr` config) with `OCR:` prompt. Falls back to vision model with verbose extraction prompt if no OCR model configured. |

Results are cached — the same PDF in subsequent turns is served from cache.

### Codex CLI

Codex handles PDFs **client-side** using local shell tools:

1. Codex tries `pdftotext` for text extraction
2. If that fails (scanned PDF), Codex extracts page images with `pdfimages`
3. Codex calls `view_image` on each page image
4. The proxy processes each `view_image` result through the **OCR model** (tool-role image)

The proxy's role is step 4 — converting page images to text via the OCR pipeline. Steps 1-3 happen on the client machine.

### OpenCode / Qwen Code

These clients handle PDFs entirely client-side. The proxy's PDF pipeline runs for direct Chat Completions requests if the request body contains PDF content signatures, but these clients typically use their own file handling tools.

### Supported PDF types

| PDF type | Claude Code | Codex CLI |
|---|---|---|
| **Native text PDF** | Stage 1: proxy extracts text | Client: `pdftotext` extracts text |
| **Scanned/image PDF** | Stage 2: OCR model extracts text | Client extracts images → proxy OCR via `view_image` |
| **Mixed PDF** (text + scanned pages) | Stage 1 for text pages, Stage 2 for scanned pages | Client handles both paths |

## Web search

When a client includes a web search tool, the proxy intercepts it, executes the search, and injects results into the conversation. The backend model sees only the final response with search context incorporated.

### Supported providers

The proxy auto-detects the search provider from the `web_search_key` prefix:

| Provider | Key prefix | Free tier | Notes |
|---|---|---|---|
| [Tavily](https://tavily.com/) | `tvly-` | 1,000 req/month | Includes AI-generated answer summary |
| [Brave Search](https://brave.com/search/api/) | `BSA` | $5/month credit (~1,000 req) | Independent index, privacy-focused |

### Per-client behavior

| Client | Search tool type | Proxy action |
|---|---|---|
| **Claude Code** | `web_search_20250305` (server tool) | Strip tool, inject `web_search` function tool, execute search, emit `server_tool_use` + `web_search_tool_result` blocks |
| **Codex CLI** | `web_search` (server tool) | Strip tool, inject `web_search` function tool, execute search, emit `web_search_call` output items |
| **OpenCode** | MCP tool via `/mcp/sse` | Proxy serves MCP endpoint with `web_search` tool |
| **Qwen Code** | Client-side function tool | No proxy involvement — client calls Tavily/Google directly |

## Recommended models

| Role | Recommended | Parameters | Notes |
|---|---|---|---|
| **Vision** | [Qwen3-VL-8B](https://huggingface.co/Qwen/Qwen3-VL-8B-Instruct) | 8B | Best quality/speed balance for image description |
| **OCR** | [PaddleOCR-VL-1.5](https://huggingface.co/PaddlePaddle/PaddleOCR-VL-1.5) | 0.9B | Purpose-built for document parsing, 94.5% accuracy, ~2s/page |
| **OCR (alt)** | [DeepSeek-OCR 2](https://huggingface.co/deepseek-ai/DeepSeek-OCR) | 3B | Higher accuracy (97%), layout analysis, table extraction |

## Caching

All pipeline results are cached by content hash for the lifetime of the proxy process:

- **Image descriptions**: cached per image URL hash + mode (`:v` or `:o`)
- **PDF text extraction**: cached per PDF content hash
- **Vision model OCR fallback**: cached per PDF content hash

Cache is in-memory only and resets on proxy restart. There is no cache size limit — for typical usage (hundreds of images/PDFs per session) memory impact is negligible.

## Pipeline flow

```
Client request
  → Protocol handler parses request
  → Translate to Chat Completions (if needed)
  → Pipeline: describe images (vision model)
  → Pipeline: OCR tool-output images (OCR model)
  → Pipeline: extract PDF text / OCR scanned pages
  → Pipeline: inject web search function tool
  → Send to backend
  → Pipeline: execute web search if called, re-send with results
  → Translate response back to client protocol
  → Stream to client
```
