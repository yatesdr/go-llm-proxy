# Changelog

## Unreleased

### Added
- Anthropic Messages API support: proxy now accepts requests on `/v1/messages`
- Anthropic-style client authentication via `x-api-key` header (in addition to `Authorization: Bearer`)
- Backend type configuration (`type: anthropic`) for models using the Anthropic Messages API
  - Sends `x-api-key` header upstream instead of `Authorization: Bearer`
  - Forwards `Anthropic-Version` and `Anthropic-Beta` request headers to upstream
  - Forwards `Request-Id` response header from upstream
  - Preserves `/v1` in upstream path (Anthropic convention: base URL omits `/v1`)
- Explicit `/anthropic/` route prefix: clients can use `base_url: https://host/anthropic` to target anthropic backends
  - Validates that the resolved model is `type: anthropic`, returns 400 otherwise
  - Allows Anthropic SDKs to use their standard base URL convention
- Config validation for `type` field (must be `""`, `"openai"`, or `"anthropic"`)
