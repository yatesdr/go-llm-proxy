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

### System dependencies

For optimal scanned-PDF processing, the host (or Docker container) should have one of:

- **`poppler-utils`** (preferred) — provides `pdftoppm`, fast and purpose-built for PDF rendering
- **`ghostscript`** — provides `gs`, widely available alternative

```bash
# Ubuntu/Debian
sudo apt install poppler-utils

# Alpine (Docker)
apk add --no-cache poppler-utils
```

The proxy uses these to rasterize scanned PDFs into PNG pages before sending them to the dedicated OCR model (e.g., paddleOCR-VL). Without a rasterizer, scanned PDFs fall through to the vision model with the raw PDF bytes — still functional but slower and dependent on the vision model accepting PDF input directly. The proxy logs `"PDF rasterization unavailable or failed, skipping OCR stage"` when no rasterizer is found.

## Image processing

When a client sends an image to a text-only backend, the proxy intercepts the image, sends it to a vision-capable model for description, and replaces the image with the text description before forwarding to the backend.

### How images are handled per role

| Image source | Processing | Pipeline | Prompt |
|---|---|---|---|
| **User-attached image** (user message) | Vision description | `vision` model only — no cascade | Describe the image accurately and objectively |
| **Tool output image** (tool message — Codex `view_image`, screenshots) | OCR → vision cascade | `ocr` model first, then `vision` on failure | `OCR:` (dedicated OCR) then verbose extraction prompt on fallback |

User-attached photos receive only vision description — dedicated OCR models produce unreliable output on natural photographs. Text visible in photos is captured adequately by the vision model's description.

Tool output images (PDF page renders, screenshots, Codex `view_image` results) first go to the dedicated OCR model. If OCR returns empty, errors, or the `ocr` model is unavailable, the proxy automatically retries via the `vision` model. When the operator has configured only one processor, the pipeline detects the overlap and avoids duplicate calls to the same backend.

### Output format

Injected content is wrapped in XML-like tags so target models clearly distinguish pipeline-sourced text from user-authored content:

- `<image_description>...</image_description>` — user-role vision description
- `<page_text>...</page_text>` — tool-role OCR/vision extraction
- `<pdf_content filename="..." source="text|ocr|vision">...</pdf_content>` — PDF extraction (see below)

### Processing details

- Images are processed **concurrently** (up to 5 in parallel)
- Successful results are **cached by content hash** — follow-up turns with the same image are instant
- Failed extractions are cached for **5 minutes** so transient upstream issues don't permanently block an image but a misbehaving client can't hammer the cascade every turn
- Maximum **10 images per request** — additional images get a placeholder
- Cache keys include a mode suffix (`:v` for vision, `:o` for OCR, `:fail` for the short-TTL failure marker) so results are stored independently
- Reasoning/thinking is disabled for vision model calls to maximize output quality

## PDF processing

PDF handling depends on the client and the PDF content type.

### Claude Code (Anthropic Messages API)

Claude Code sends PDF content as base64 `document` blocks. The proxy handles them in a three-stage cascade:

| Stage | Condition | Action |
|---|---|---|
| **Stage 1: Text extraction** | Always attempted | Pure Go text extraction via `ledongthuc/pdf`. Fast, accurate for native PDFs. If ≥ 50 characters of plain text are recovered, subsequent stages are skipped. |
| **Stage 2: OCR model** | Stage 1 returned too little text (scanned/image PDF) | Send PDF to the configured `ocr` model. On HTTP error or empty response, fall through to Stage 3. |
| **Stage 3: Vision fallback** | Stage 2 failed or the only processor configured is `vision` | Send PDF to the configured `vision` model with the verbose extraction prompt. Covers scanned PDFs that dedicated OCR backends reject (e.g., paddleOCR does not accept `data:application/pdf` input). |

The injected text block is tagged `<pdf_content filename="..." source="text|ocr|vision">...</pdf_content>` — the `source` attribute identifies which stage produced the content, so downstream logs and debugging sessions can see the pipeline decision without replaying it.

Successful extractions are cached permanently (keyed on the PDF's content hash). Total failures are cached for 5 minutes so a broken upstream doesn't permanently block a document but a misconfigured client can't trigger repeated cascade attempts every turn.

### Chat Completions API (any client)

OpenAI Chat Completions has no standard PDF input shape, but clients often submit PDFs as `image_url` with a `data:application/pdf;base64,...` URL. The proxy normalizes these into the same `pdf_data` block Anthropic produces, so Chat Completions clients go through the exact same three-stage cascade as Claude Code — no client-side changes required.

### Codex CLI (Responses API)

Codex typically handles PDFs **client-side** using local shell tools:

1. Codex tries `pdftotext` for text extraction
2. If that fails (scanned PDF), Codex extracts page images with `pdfimages`
3. Codex calls `view_image` on each page image
4. The proxy processes each `view_image` result through the **tool-role image cascade** (OCR first, vision fallback on failure)

The proxy's role is step 4. When Codex instead submits a PDF directly as an `input_image` with a `data:application/pdf` URL — seen in some third-party integrations — the Responses-API translator converts it to a `pdf_data` block so the full three-stage cascade runs on the proxy side.

### OpenCode / Qwen Code

These clients handle PDFs entirely client-side. The proxy's PDF pipeline runs for direct Chat Completions requests if the request body contains PDF content signatures, but these clients typically use their own file handling tools.

### Supported PDF types

| PDF type | Claude Code / Chat Completions | Codex CLI |
|---|---|---|
| **Native text PDF** | Stage 1: proxy extracts text | Client: `pdftotext` extracts text |
| **Scanned/image PDF** | Stage 2 OCR → Stage 3 vision cascade | Client extracts images → proxy OCR → vision cascade per page |
| **Mixed PDF** (text + scanned pages) | Stage 1 for text pages, Stages 2/3 for scanned pages | Client handles both paths |

## Web search

When a client includes a web search tool, the proxy intercepts it, executes the search, and injects results into the conversation. The backend model sees only the final response with search context incorporated.

### Supported providers

The proxy auto-detects the search provider from the `web_search_key` prefix:

| Provider | Key prefix | Free tier | Notes |
|---|---|---|---|
| [Tavily](https://tavily.com/) | `tvly-` | 1,000 req/month | Includes AI-generated answer summary |
| [Brave Search](https://brave.com/search/api/) | `BSA` | $5/month credit (~1,000 req) | Independent index, privacy-focused |

### Per-client behavior

| Client | Proxy-side (Tavily or Brave) | Client-side fallback |
|---|---|---|
| **Claude Code** | Automatic — proxy intercepts `web_search_20250305` server tool | Tavily only (via MCP) |
| **Codex CLI** | Automatic — proxy intercepts `web_search` server tool | Tavily only (via MCP) |
| **OpenCode** | Automatic — proxy serves `/mcp/sse` endpoint | Tavily only (via MCP) |
| **Qwen Code** | Via MCP — proxy serves `/mcp/sse` endpoint | Tavily, Google, or DashScope |

**Note:** Brave Search is only available through the proxy (`web_search_key` in config.yaml). Client-side search configs only support Tavily because there is no Brave MCP endpoint. If you want Brave Search, configure it on the proxy and all clients will use it automatically via their respective mechanisms.

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
