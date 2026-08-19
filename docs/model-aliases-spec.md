# Model aliases + unknown-model capture — spec (2026-08-19)

Owner: unassigned. Sponsor: Derek. Reviewer: Fable.

## Why

OpenAI-pointed tools call the proxy with model ids we never configured —
the concrete case: Codex CLI's automatic-approval reviewer calls
`/v1/responses` with its own default model id, gets `404 unknown model`,
and every escalated command (e.g. `git commit`) is denied. The operator
fix should be: open the admin, see the unrecognized id with a request
count, pick an existing model from a dropdown, click Alias — and every
such tool "just works" from then on. Aliases are also generally useful
(e.g. pointing a reviewer id at a smaller/faster model than the main
session model).

## Feature A — model aliases (config + resolution)

- `Config.Aliases map[string]string` (YAML `aliases:` mapping,
  alias → target model name).
- Validation in `validateConfig`: every target must name an existing
  entry in `models:`; an alias key must not equal any model name; an
  alias may not point at another alias (single-level, no chains).
- `config.ResolveModelName(cfg, name) string`: exact model-name match
  returns name; else alias lookup returns the target; else returns name
  unchanged. Case-sensitive (OpenAI ids are).
- **Handler ordering fix (all four call sites):** today
  `auth.KeyAllowsModel(key, req.Model)` runs BEFORE `config.FindModel`
  (`proxy.go:~101`, `responses.go:~235`, `messages.go:~97`,
  `count_tokens.go:~75`). Change each to: resolve canonical name first,
  authorize against the CANONICAL name, then `FindModel(canonical)`.
  Consequence (intended): a key restricted to `[glm-5.3]` automatically
  covers aliases of glm-5.3. Usage logging records the canonical name;
  add an `alias_from` field when resolution changed the name.

## Feature B — unknown-model capture

- New `internal/handler/unknown_models.go`: a threadsafe registry —
  `Record(modelID, endpoint string)`, entries `{id, count, firstSeen,
  lastSeen, lastEndpoint}`, id length-capped at 128 chars, registry
  capped at 200 entries evicting oldest-lastSeen, `Snapshot()` sorted
  lastSeen-descending, `Remove(id)`.
- Call `Record` at each of the four `unknown model` 404 sites, with the
  endpoint label (`chat/completions`, `responses`, `messages`,
  `count_tokens`). In-memory only; resets on restart (say so in the UI
  hint). No persistence, no unbounded growth.

## Feature C — admin UI (follow admin_models.go patterns exactly)

- `ModelsData` payload gains `aliases: [{alias, target}]` and
  `unknown_models: [{id, count, last_seen, endpoint}]`.
- Models page gains one section, "Aliases & unrecognized requests":
  - unrecognized table: id, count, last seen, endpoint, a dropdown of
    existing model names, and an **Alias** button per row;
  - aliases table: alias → target with a Delete button per row;
  - hint text: unrecognized list is since-restart, capped at 200.
- Mutations via `ModelsMutate` new actions `alias_add {alias, target}` /
  `alias_delete {alias}` (same DTO/switch style). Creating an alias
  calls `Remove(id)` on the registry.
- Store methods `cs.AddAlias(alias, target)` / `cs.DeleteAlias(alias)`
  via `mutateYAML` on the `aliases` mapping node — mirror `AddModel`
  (create the mapping node when absent; validate against the CURRENT
  config first, same error style). Hot-reload then propagates as with
  every other config edit.

## /v1/models listing

`internal/handler/models.go`: after real models, `appendModel` each
alias id (context window copied from its target), included only when the
key allows the TARGET. Alias ids appear as ordinary model objects so
client pickers can select them.

## Out of scope (v1)

Rewriting the `model` field in upstream responses to echo the alias
(passthrough stays as-is — note it); wildcard/regex aliases; persistence
of the unknown-model registry; per-key alias visibility rules beyond the
target-based check.

## Tests

1. Config: validation (missing target, alias==model name, alias→alias
   chain all rejected), resolution semantics, YAML round-trip via the
   store mutators (mirror `save_test.go` incl. backup behavior).
2. Handlers: alias request end-to-end against a fake backend on all four
   endpoints (existing `proxy_test.go` patterns); restricted-key +
   alias authorization; unknown id → 404 AND registry row recorded.
3. Registry: concurrency (`-race`), cap/eviction, Remove.
4. Admin: `alias_add`/`alias_delete` mutate flow, ModelsData payload,
   listing includes alias with target's context window.

## Acceptance (the codex scenario, verified live)

1. Request an unconfigured id against `/v1/responses` → 404, id appears
   in the admin list with endpoint `responses`.
2. Alias it to an existing model in the UI → same request returns 200;
   registry row gone; alias survives restart (it is in config.yaml).
3. On llm.eidonix.com: pull the actual reviewer id from the access log,
   alias it, and confirm a codex `git commit` escalation passes its
   automatic approval review end-to-end.

## Rules

Follow existing patterns exactly (mutateYAML, DTO structs, slog,
httputil errors); no new dependencies; no behavior change for known
model ids; bounded everything; `go test ./... && go vet ./...` green.
Deploy note for Derek: a proxy binary restart interrupts live agent
streams — deploy at a quiet moment; alias edits afterward are hot-reload
and never need a restart.
