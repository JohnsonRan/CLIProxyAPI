# Downstream Fork Maintenance Guide (`JohnsonRan/CLIProxyAPI`)

This repository is a maintained downstream distribution of [`router-for-me/CLIProxyAPI`](https://github.com/router-for-me/CLIProxyAPI). It tracks `upstream/main` and layers independently reviewable, buildable, and testable patches on top of the latest upstream baseline.

## Branch Architecture

- **Upstream baseline**: `https://github.com/router-for-me/CLIProxyAPI.git` (`main`)
- **`ci`**: Downstream maintenance overlay carrying this `MAINTAIN.md`, downstream `README.md` notice, `.github/workflows/upstream-sync.yml`, `.github/workflows/ghcr-docker.yml`, the combination-conflict resolver `.github/scripts/resolve-union-conflict.sh`, and multi-patch integration tests (`sdk/api/handlers/downstream_client_access_combination_test.go`; it compiles only on the integrated tree). Never mix product runtime changes into `ci`.
- **`patch/<name>`**: Independent product patch branches, each branched directly from `upstream/main` and kept suitable for standalone review and independent build/test. One patch carries one feature or fix, so a patch becomes empty once upstream absorbs it.
- **`main`**: Integrated release branch rebuilt from `upstream/main` with one countable squash commit per branch (`merge ci branch` followed by each `merge patch/<name> branch` in fixed order). Never commit fixes only to `main`.

The integration order lives in `PATCH_BRANCHES` in `.github/workflows/upstream-sync.yml`. Keep it identical to the list below.

## Active Patch Branches (Fixed Integration Order)

1. **`patch/http-compression`**
   - **Goal**: End-to-end HTTP, SSE, and WebSocket compression (`gzip`, `br`, `zstd`, `deflate`) across both downstream and upstream legs.
   - **Scope**:
     - Downstream request body decompression (`sdk/api/handlers/request_body.go`, `internal/api/middleware/request_logging.go`, and the Claude, Gemini and `/v1/alpha/search` handlers) supporting `gzip`, `br`, `zstd`, `deflate`, multi-coding chains, and magic-byte `gzip`/`zstd` detection. Decoded size is capped by `max-request-body-bytes`, or by default at 256 MiB in handlers and 32 MiB in the pre-auth request log copy; zstd request windows are capped at 8 MiB; `/v1/alpha/search` keeps its 16 MiB ceiling.
     - Downstream HTTP & SSE response compression middleware (`internal/api/middleware/response_compression.go`) negotiating `zstd > br > gzip > deflate`, flushing stream encoders on every SSE flush, with small per-stream encoders (zstd 1 MiB window, brotli quality 5 / 256 KiB window), skipping image/video/audio/archive bodies and 206 responses and weakening strong `ETag`s, plus `EnableCompression: true` on `wsrelay` and `codex/live` WebSocket upgraders/dialers.
     - Upstream HTTP response decompression (`internal/runtime/executor/helps/response_compression.go`) across `codex`, `openai-compatibility`, `gemini`, `antigravity`, `kimi`, `xai`, and `meta` executors. zstd bodies are decoded synchronously (`WithDecoderConcurrency(1)`, about 2x faster and a third of the allocations of the default four-worker decoder on SSE streams) with the window capped at 64 MiB.
     - Upstream request body compression (`upstream.codex.upstream-request-compression`) defaulting to `zstd` level 3 for `chatgpt.com` `/responses` and `/responses/compact` bodies of at least 1024 bytes.
   - **Related Upstream Work**: PR [#5132](https://github.com/router-for-me/CLIProxyAPI/pull/5132), PR [#4461](https://github.com/router-for-me/CLIProxyAPI/pull/4461).

2. **`patch/responses-ws-sse-incremental`**
   - **Goal**: HTTP incremental SSE, standard/incremental WebSocket normalization, and the HTTP-to-WebSocket upstream bridge.
   - **Scope**:
     - `permessage-deflate` on the `/v1/responses` WebSocket upgrader.
     - WebSocket request normalization: default omitted `"type"` to `"response.create"`, coerce string `"input"`, inherit `tools`, `tool_choice`, and `parallel_tool_calls` across incremental turns, and keep prewarm `additional_tools`.
     - HTTP continuation (`openai_responses_http_incremental.go`): `POST /v1/responses` with `previous_response_id` plus delta `input` expands the cached history for stateless upstreams. Entries are bound to the creating caller (access provider and client key), expire after 30 minutes, and are capped at 1024 entries and 256 MiB.
     - HTTP-to-WebSocket bridge: the global `oauth.providers.codex.enable-websocket-upstream` switch, or a per-credential `websockets: true`, routes HTTP/SSE clients through the Codex WebSocket upstream while `/responses/compact` and `/v1/images/*` stay on HTTP. **Downstream semantics**: upstream's per-credential `websockets` only affects WebSocket clients; here it also bridges HTTP clients and overrides the global switch. The panel patch `patch/websocket-upstream-toggle` exposes the global switch and documents this.
   - **Related Upstream Work**: PR [#4907](https://github.com/router-for-me/CLIProxyAPI/pull/4907), PR [#3562](https://github.com/router-for-me/CLIProxyAPI/pull/3562), PR [#5420](https://github.com/router-for-me/CLIProxyAPI/pull/5420), Issue [#2596](https://github.com/router-for-me/CLIProxyAPI/issues/2596).

3. **`patch/chatgpt-utls-pool`**
   - **Goal**: Reuse ChatGPT uTLS HTTP/2 connections and survive transient connection setup failures.
   - **Scope**: `internal/runtime/executor/helps/utls_client.go` pools one HTTP/2 connection per host and proxy with a TLS session cache, retries dial/handshake failures up to 3 times before any request byte is sent (an exhausted retry returns the raw transport error, so credentials are not cooled), retries a pooled request on a new connection only when it was never sent (unusable connection or `REFUSED_STREAM`, body rewound through `GetBody`), retires replaced or failed connections gracefully, and closes idle pooled connections after 90 seconds.
   - **Related Upstream Work**: PR [#5261](https://github.com/router-for-me/CLIProxyAPI/pull/5261), PR [#6398](https://github.com/router-for-me/CLIProxyAPI/pull/6398).

4. **`patch/runtime-memory-bounds`**
   - **Goal**: Bound long-lived memory.
   - **Scope**: Expire WebSocket tool-call and tool-output replay sessions after 1 idle hour unless their WebSocket is still connected (`openai_responses_websocket_toolcall_repair.go`), and cap retained Codex `response.output_item.done` items for `response.completed` reconstruction at 16 MiB: past it, `image_generation_call` items (already streamed) are omitted first, and all items only if that is not enough (`codex_executor_terminal.go`, `codex_executor_stream.go`).
   - **Related Upstream Work**: PR [#5804](https://github.com/router-for-me/CLIProxyAPI/pull/5804).

5. **`patch/startup-model-registration`**
   - **Goal**: Avoid `model_not_found` for requests that arrive before the watcher's first load.
   - **Scope**: `Service.Run` syncs the plugin model runtime under `configRuntimeMu` before the listener opens, in every mode (`sdk/cliproxy/service_lifecycle.go`).
   - **Related Upstream Work**: PR [#6461](https://github.com/router-for-me/CLIProxyAPI/pull/6461).

6. **`patch/claude-api-key-policy`**
   - **Goal**: Official Anthropic API key handling.
   - **Scope**: An empty or `auto` cloak block does not count as explicit cloaking, so official keys keep their native wire format; `400 "credit balance is too low"` becomes a credential-scoped `402` (`claude_executor_cloaking.go`, `claude_executor_request.go`).
   - **Related Upstream Work**: PR [#6464](https://github.com/router-for-me/CLIProxyAPI/pull/6464).

7. **`patch/management-panel-update`**
   - **Goal**: Let the management panel install the latest panel asset on demand.
   - **Scope**: `POST /v8/management/server/update-panel` (`internal/api/handlers/management/panel_update.go`) calls `managementasset.ForceUpdateManagementHTML`, which shares the auto-updater's singleflight key, verifies the release digest, and returns `409 control_panel_disabled` when `remote-management.disable-control-panel` is set.
   - **Cross-repo**: Required by the panel patch `patch/update-management-panel`.
   - **Note**: The User-Agent shim moved to `patch/management-ua-shim`.

8. **`patch/management-ua-shim`** (transitional)
   - **Goal**: Keep saved panel logins readable across browser updates when serving panel builds that key their localStorage obfuscation on the full User-Agent.
   - **Scope**: `internal/managementasset/ua_shim.go` injects a script into such `management.html` builds that re-keys stored values onto a version-free User-Agent once (`cpa_obfuscation_ua_migrated` marker) and pins `navigator.userAgent`. `serveManagementControlPanel` serves unchanged files through `http.ServeFile`.
   - **Cross-repo**: The panel patch `patch/secure-storage-stable-key` fixes the key in the panel itself and ships the marker, so this shim leaves those builds alone.
   - **Retire when**: Users no longer run panel builds without `patch/secure-storage-stable-key`, including upstream panels selected through `remote-management.panel-github-repository`. Removing it earlier makes those panels unreadable for browsers it already migrated.

9. **`patch/downstream-panel-source`**
   - **Goal**: Serve this fork's management panel by default.
   - **Scope**: `DefaultPanelGitHubRepository`, the default release lookup in `internal/managementasset/updater.go`, and `config.example.yaml` point at `JohnsonRan/Cli-Proxy-API-Management-Center`. An explicit `remote-management.panel-github-repository` still wins. The last-resort fallback page (`cpamc.router-for.me`) stays upstream's.

10. **`patch/fix-interrupt-test-race`**
   - **Goal**: Deflake `TestResponsesInterruptStopsHTTPUpstream`, which sampled a cancellation flag once and failed about one run in thirty.
   - **Scope**: Test-only (`sdk/api/handlers/openai/openai_responses_interrupt_test.go`). Retire when upstream fixes the test.

11. **`patch/claude-thinking-off`**
    - **Goal**: Turn Claude thinking off the way each model accepts.
    - **Scope**: `claude-sonnet-5-5` uses `thinking.type: "between_tools"`; `claude-opus-5-5`, `claude-fable-5`, `claude-fable-5-1` cannot turn thinking off (`DisableType: "none"`) and fall back to their lowest adaptive level with `display: "omitted"`; other Claude-format models keep `thinking.type: "disabled"`; dated and bracketed Claude variants resolve to their catalog entry. The facts live in `internal/registry/claude_thinking_facts.go` and are overlaid on every loaded catalog, because the release workflow and the runtime updater replace `models.json` with the remote catalog.
    - **Related Upstream Work**: PR [#6571](https://github.com/router-for-me/CLIProxyAPI/pull/6571), PR [#6570](https://github.com/router-for-me/CLIProxyAPI/pull/6570).

12. **`patch/claude-search-recovery`**
    - **Goal**: Recover Claude conversations after OAuth credential failover when an earlier turn's encrypted `web_search_tool_result` is rejected with HTTP 400.
    - **Scope**: `internal/runtime/executor/helps/claude_search_recovery.go` and the Claude execute/stream paths, applied before user payload rules.
    - **Related Upstream Work**: PR [#6576](https://github.com/router-for-me/CLIProxyAPI/pull/6576).

13. **`patch/codex-oauth-resilience`**
    - **Goal**: Codex OAuth refresh resilience.
    - **Scope**: Cache completed refreshes for 5 minutes for late concurrent callers, stop retrying terminal `invalid_grant`/`refresh_token_invalidated`, send the Codex `User-Agent` on exchange/refresh (`internal/auth/codex/openai_auth.go`); classify `401 Your IP is not authorized` as a retryable `503 ip_temporarily_restricted` so it does not force a refresh.
    - **Related Upstream Work**: PR [#6447](https://github.com/router-for-me/CLIProxyAPI/pull/6447), PR [#6437](https://github.com/router-for-me/CLIProxyAPI/pull/6437), PR [#6165](https://github.com/router-for-me/CLIProxyAPI/pull/6165), PR [#5766](https://github.com/router-for-me/CLIProxyAPI/pull/5766), PR [#5850](https://github.com/router-for-me/CLIProxyAPI/pull/5850).

14. **`patch/embeddings-and-rerank`**
    - **Goal**: OpenAI-compatible `/v1/embeddings`, `/v1/embedding`, `/v1/rerank`, and `/rerank` for OpenAI-compatible and Gemini providers.
    - **Scope**: Routes, handlers, `OpenAICompatExecutor` embeddings/rerank execution, Gemini `:embedContent`/`:batchEmbedContents` translation, and rerank usage publication (OpenAI `usage` or `meta.tokens`/`meta.billed_units`). The model-level `embedding` and `rerank` flags are metadata for the panel's connectivity test and set the listed output modality to `embeddings` / `rerank` unless `output-modalities` is configured; routing does not read them.
    - **Usage switch**: `observability.usage.exclude-embeddings: true` (hot-reloaded, default off, ignored in Home mode) keeps `/v1/embeddings` and `/v1/embedding` records, successes and failures, out of the usage queue that the panel and cpa-usage-keeper read. `ExecuteEmbeddingWithAuthManager` tags the context with `usage.RequestKindEmbeddings` (new `sdk/cliproxy/usage/request_kind.go`); the queue plugin drops tagged records. Request logs, application logs and other usage plugins still see them. Rerank is not affected.
    - **Related Upstream Work**: Issue [#5961](https://github.com/router-for-me/CLIProxyAPI/issues/5961), Issue [#3641](https://github.com/router-for-me/CLIProxyAPI/issues/3641), Issue [#1084](https://github.com/router-for-me/CLIProxyAPI/issues/1084), PR [#1191](https://github.com/router-for-me/CLIProxyAPI/pull/1191).

15. **`patch/client-api-key-models`**
    - **Goal**: Visible and usable model access control globally and per client API key.
    - **Scope**:
      - `access.models` / `access.allowed-models` / `access.excluded-models` and per-key mapping entries in `access.api-keys`.
      - Filter `/v1/models` and model detail endpoints; reject execution, count-tokens, `/v1/alpha/search` and Codex Realtime (checked on the requested model, before the upstream rewrite) with `403`; `auto` is also checked against the model it resolves to.
      - Struct-based config saves fold per-key rules back onto `access.api-keys` (`foldClientKeysIntoAPIKeys`); comments attached to those mapping entries are not preserved.
      - `GET /v8/management/models` lists servable models without client-key filtering.
    - **Known limits**: Rules match the requested model name, before alias and prefix resolution. A key with its own allow-list does not inherit the global exclusion list.
    - **Cross-repo**: Required by the panel patch `patch/client-api-key-models`.
    - **Related Upstream Work**: PR [#6568](https://github.com/router-for-me/CLIProxyAPI/pull/6568), PR [#4030](https://github.com/router-for-me/CLIProxyAPI/pull/4030), PR [#5517](https://github.com/router-for-me/CLIProxyAPI/pull/5517).

16. **`patch/proxy-transport-reuse`**
    - **Goal**: Reuse proxied upstream connections and make SOCKS5 dials cancellable.
    - **Scope**:
      - `cachedProxyTransport` (`internal/runtime/executor/helps/proxy_helpers.go`) keeps one `http.Transport` per proxy URL in a bounded LRU for `NewProxyAwareHTTPClient` and the non-Anthropic/non-ChatGPT fallback of `NewUtlsHTTPClient`. Before, every request with a request, credential, or global proxy URL built a new transport: a fresh TCP, proxy, and TLS handshake per call, with the discarded idle sockets left open for 90 seconds. The per-proxy scope matches upstream's uTLS round-tripper caches and conductor round-tripper provider.
      - SOCKS5 transports (`sdk/proxyutil/proxy.go`) dial through the dialer's `DialContext`, so a canceled request or an unresponsive proxy no longer blocks until the OS TCP timeout.
    - **Related Upstream Work**: PR [#1826](https://github.com/router-for-me/CLIProxyAPI/pull/1826) (closed by the maintainer: "No plans to support this"), PR [#4592](https://github.com/router-for-me/CLIProxyAPI/pull/4592) (closed unmerged), PR [#4604](https://github.com/router-for-me/CLIProxyAPI/pull/4604) (withdrawn by its author as fork-specific), PR [#2707](https://github.com/router-for-me/CLIProxyAPI/pull/2707) (closed unmerged; raised `MaxIdleConnsPerHost` instead).
    - **Retire when**: Upstream stops building proxy transports per request. Upstream declined the earlier attempts, so expect to carry this indefinitely.

17. **`patch/systemone-endpoint`**
    - **Goal**: Serve TypeSafe System One decision models (Jev) on `POST /v1/systemone` (request/response shape: https://docs.typesafe.ai/api).
    - **Scope**: `SystemOne` handler (`sdk/api/handlers/openai/openai_systemone_handlers.go`) validates `model`, `state` and `questions` and routes by model with entry protocol `systemone`. `OpenAICompatExecutor.executeSystemOne` (`internal/runtime/executor/openai_compat_executor_systemone.go`) forwards the body unchanged to `<base-url>/systemone`, rewriting only the upstream model name before payload rules (protocol `systemone`) run last; upstream status/body are preserved and `usage.input_tokens`/`output_tokens` are recorded. Command Code OAuth credentials (`patch/commandcode-oauth`) list `typesafe/jev` themselves. Otherwise configure as an `openai-compatibility` provider: TypeSafe `https://api.typesafe.ai/v1` (`jev-latest`) or Command Code `https://api.commandcode.ai/provider/v1` (`typesafe/jev`). The model field `systemone: true` (`OpenAICompatibilityModel.SystemOne`) is metadata for management clients and sets the listed output modality to `decisions` unless `output-modalities` is configured; routing does not read it.
    - **Cross-repo**: The panel patch `patch/systemone-endpoint` exposes the `systemone` flag and tests flagged models against `/systemone`.
    - **Related Upstream Work**: Issue [#6012](https://github.com/router-for-me/CLIProxyAPI/issues/6012) (closed as completed without code on `main`), PR [#6589](https://github.com/router-for-me/CLIProxyAPI/pull/6589) (open, unreviewed; generic native plugin routes plus the external `EcoRayDev/cpa-plugin-typesafe` plugin). This fork carries the smaller built-in passthrough instead.
    - **Retire when**: Upstream serves `/v1/systemone` natively, or PR #6589 lands and the plugin route is adopted here.

18. **`patch/model-capabilities`**
    - **Goal**: Let clients classify models and read their limits and reasoning support from the plain model list alone, without guessed values.
    - **Scope**:
      - `/v1/models` entries carry OpenRouter-style `architecture.input_modalities`, `architecture.output_modalities`, `architecture.modality` and `context_length` from the registry (`internal/registry/model_architecture.go`). Unknown modalities are omitted, never guessed.
      - Output modalities use OpenRouter names: `text`, `image`, `embeddings`, `rerank`, `decisions`. Built-in models take them from `models.json`; configured `image: true` models default to `[image]`; patches 14 and 17 map `embedding`, `rerank` and `systemone`. Explicit `output-modalities` always wins. Keep config flags named after endpoints (`systemone`, not `decisions`); only the listing uses the modality vocabulary.
      - The Codex client catalog (`/v1/models?client_version=...`) marks models whose outputs are only `embeddings`, `rerank` or `decisions` with `visibility: hide`, like image and speech models.
      - Plain `/v1/models` also carries, when declared: `name` (display name) and `top_provider.max_completion_tokens` (OpenRouter format). `context_length` and the output limit fall back to `inputTokenLimit`/`outputTokenLimit` for Gemini-style catalogs; the configured `max-context-length` override is folded into `context_length` and keeps priority. The registry exposes the limits as separate `input_token_limit`/`output_token_limit` map keys so the Codex catalog, which reads `context_length`/`max_completion_tokens`, is unchanged.
      - `reasoning` is tri-state (`internal/registry/model_reasoning.go`): absent = unknown; `{"supported": false}` = the proxy strips reasoning (registered non-user-defined model without `Thinking`, or explicitly empty thinking config); otherwise `{"supported": true, "levels": [...], "budget": {min, max, zero_allowed, dynamic_allowed}}` with levels as wire values and each part only when declared. User-defined models without explicit thinking stay unknown, and the low/medium/high fallback applied to openai-compatibility models without a thinking config is marked `ModelInfo.DefaultThinking` and never advertised. User-defined models matched to the static catalog report catalog levels that the proxy passes through unvalidated.
      - All additions are additive; unknown values are omitted rather than guessed.
    - **Cross-repo**: `JohnsonRan/pi-cliproxyapi-provider` registers models by `architecture.output_modalities` (decisions as Pi classifiers). `JohnsonRan/dsh-cliproxyapi-provider` (DeepSeek Harness) reads the plain list instead of the Codex catalog, whose template fallback guesses modalities, reasoning levels and context windows for uncatalogued models.
    - **Related Upstream Work**: Issue [#5119](https://github.com/router-for-me/CLIProxyAPI/issues/5119) (closed as not planned: the maintainer keeps the default `/v1/models` strictly in OpenAI format and points to `?client_version=1` for extended metadata, so this patch is expected to stay downstream; extending the default list is a deliberate choice because generic OpenRouter-style readers cannot add query parameters), PR [#5646](https://github.com/router-for-me/CLIProxyAPI/pull/5646) (open, unreviewed; separate `?capabilities=true` schema bundled with unrelated token-count and refresh changes, no embeddings/rerank/decisions; do not reuse that parameter name), PR [#5801](https://github.com/router-for-me/CLIProxyAPI/pull/5801) (capability propagation across aliases), PR [#3371](https://github.com/router-for-me/CLIProxyAPI/pull/3371) (closed unmerged; full metadata and thinking levels in the plain list), PR [#5123](https://github.com/router-for-me/CLIProxyAPI/pull/5123) and PR [#6194](https://github.com/router-for-me/CLIProxyAPI/pull/6194) (open; token limits and `inputTokenLimit` fallback in the plain list), PR [#4608](https://github.com/router-for-me/CLIProxyAPI/pull/4608) and PR [#5246](https://github.com/router-for-me/CLIProxyAPI/pull/5246) (open; output limits in the Codex catalog only).
    - **Retire when**: Upstream lists output modalities, limits and reasoning support in `/v1/models`; then move the flag mappings onto upstream's field names.

19. **`patch/quota-cooldown-fail-fast`**
    - **Goal**: Stop clients from retrying, or sleeping through `Retry-After`, when every credential for a model is out of quota for hours or days.
    - **Scope**: Carries PR #6647's two commits unchanged. A model cooldown longer than `QuotaExhaustedCooldownThreshold` (5 minutes, `sdk/cliproxy/auth/selector.go`) keeps `code: "model_cooldown"` and adds `type: "usage_limit_reached"`, `resets_at` and `retryable: false`; it drops `Retry-After` and sets `X-Should-Retry: false`, which the official OpenAI and Anthropic SDKs honor. The Claude handler maps the type to `rate_limit_error`. `handlers.WriteSafeRetryHeaders` forwards the header on the base, Claude, Codex alpha search and Codex live/realtime selection-error paths, and CORS exposes it. Short cooldowns are unchanged.
    - **Known limits**: The threshold reads the cooldown length, not its cause. A quota cooldown without an upstream reset time grows by backoff (up to 30 minutes), so after enough consecutive 429s a guessed backoff above 5 minutes is also reported as a final usage limit. This is the carried PR's behavior.
    - **Related Upstream Work**: Issue [#6646](https://github.com/router-for-me/CLIProxyAPI/issues/6646), PR [#6647](https://github.com/router-for-me/CLIProxyAPI/pull/6647) (open; its second commit fixes the live/realtime gap Copilot reported), PR [#3244](https://github.com/router-for-me/CLIProxyAPI/pull/3244) (transient cooldown retry hints).
    - **Retire when**: PR #6647 or an equivalent lands.

20. **`patch/responses-input-tokens`**
    - **Goal**: Serve OpenAI's `POST /v1/responses/input_tokens` (also on `/backend-api/codex/responses/input_tokens`).
    - **Scope**: `sdk/api/handlers/openai/openai_responses_input_tokens.go` applies the same boundary rewrites as `/v1/responses` (multi-agent-v2 tools, orphan delegation), drops `stream`, and runs the selected executor's `CountTokens`. No translator targets `openai-response` for token counts, so the handler reads every built-in count location (`input_tokens`, `response.usage.input_tokens`, `usage.prompt_tokens`, `totalTokens`, `total_tokens`, `usage.total_tokens`) and answers `{"object":"response.input_tokens","input_tokens":N}`. It never re-estimates a provider's number; an unrecognized count is a `502`.
    - **Related Upstream Work**: PR [#5757](https://github.com/router-for-me/CLIProxyAPI/pull/5757) (open, conflicting). This fork reimplements it instead of carrying it because that PR's review findings are valid: it ignored the Gemini and OpenAI-compatible count shapes, skipped the boundary rewrites, and fell back to tokenizing raw JSON. Its fourth finding (string `input` counted as zero) does not apply: the Codex request translator wraps string input in a message.
    - **Known limits**: When the count is unrecognized (only possible for plugin executors), request lifecycle hooks have already recorded success while the client gets `502`.

21. **`patch/openai-compat-model-discovery`**
    - **Goal**: Let an `openai-compatibility` provider advertise its upstream models without listing them, and make `excluded-models` work there.
    - **Scope**: Carries PR #5898. `auto-discover-models: true` fetches `<base-url>/models` with the first key (its `proxy-url`, else the global `proxy-url`, which this fork adds) and merges the IDs under configured `models`, which keep priority. `excluded-models` (exact or wildcard) filters the final list. Discovery is read on every model-registration batch (startup, config reloads, auth-file updates). Only the first fetch for a provider runs inline; afterwards a list older than 30 minutes is returned at once and refreshed by one background fetch per provider, which only updates the cache, so registration never waits on a provider's `/models`. A failed fetch keeps the last good list. The cache key covers the provider name, base URL, first key, its proxy and the headers. The fork adds the non-blocking refresh and the input-based cache key (review finding), and adapts the PR to upstream's split `newOpenAICompatibilityRegistrationCacheForConfig` builder, the second registration-cache call in `service_plugins.go`, and the v8 config layout (covered by `TestOpenAICompatibilityDiscoveryFieldsSurviveV8Layout`).
    - **Known limits**: A refreshed list takes effect at the next registration batch, not immediately. Cache entries of removed providers stay until restart. The request is bounded by a 30-second context deadline: it is a metadata call, not an upstream conversation (the AGENTS.md timeout rule), like the remote model updater, and the bound keeps a hung provider from pinning a refresh. Discovered models get the default openai-compatibility thinking levels, which `patch/model-capabilities` marks `DefaultThinking` and does not advertise.
    - **Related Upstream Work**: PR [#5898](https://github.com/router-for-me/CLIProxyAPI/pull/5898) (open, unreviewed).
    - **Retire when**: PR #5898 or an equivalent lands.

22. **`patch/client-key-provider-scope`**
    - **Goal**: Restrict client API keys to providers, so one tool's key cannot spend another provider's quota.
    - **Scope**: Carries PR #6568's five commits, plus one fork fix: the name `kimi` covers every Kimi credential key (`kimi`, `kimi-ai`, `kimi.ai`, `kimi.com`), which PR #6568 matched only as `kimi`; a specific key such as `kimi-ai` stays exact. `client.key-scopes` maps a `key-prefix` (longest match wins) to `providers`; an `openai-compatibility` name also matches `openai-compatible-<name>`. Execute, stream and count narrow providers before selection (`400 model_not_found` for a model only other providers serve); executor-plugin routes need the plugin ID; model lists, model details and Gemini `/v1beta/models/{model}` are filtered; Codex-only routes (`/v1/alpha/search`, `/v1/live`, `/v1/realtime*`) answer `403 provider_not_allowed`. Home mode is untouched.
    - **Interaction**: Separate from `patch/client-api-key-models` (`access.models` / `access.excluded-models` and per-key entries in `access.api-keys`). A key restricted by both gets the intersection, in lists and execution; see "Combination Conflicts".
    - **Related Upstream Work**: PR [#6568](https://github.com/router-for-me/CLIProxyAPI/pull/6568) (open, mergeable, review findings fixed), PR [#6569](https://github.com/router-for-me/CLIProxyAPI/pull/6569) (account pools on top of it), PR [#5660](https://github.com/router-for-me/CLIProxyAPI/pull/5660) (per-key credential prefixes).
    - **Retire when**: PR #6568 lands.

23. **`patch/openai-decisions-endpoint`**
    - **Goal**: Pass through OpenAI's Decisions API on `POST /v1/decisions`.
    - **Scope**: `Decisions` (`sdk/api/handlers/openai/openai_decisions_handlers.go`) validates `model`, `input` and a non-empty `questions` array, rejects `stream`, and routes by model with entry protocol `openai-decisions`. `helps.ExecuteOpenAIDecisions` forwards the body unchanged to `<base-url>/decisions`, rewriting only the upstream model name before payload rules (protocol `openai-decisions`) run last; upstream status and body are preserved on both executor paths and usage is recorded. Only `codex` (`codex-api-key`, default base `https://api.openai.com/v1`) and `openai-compatibility` providers are selected (`adjustExecutionProvidersForEntryProtocol`); other executors cannot handle the body. `CodexAutoExecutor` keeps decisions on HTTP even when patch 2 bridges HTTP clients to WebSockets.
    - **Failure scope**: Decisions share model names with chat (`gpt-6-luna`), so an endpoint failure must not cool down the credential or its model. Upstream 4xx and 501 answers other than 401, 402 and 429 (a relay without `/decisions` answering 404, an account without access answering 403, a bad request) are request-scoped: no cooldown, and the request does not fall back to other credentials. 401, 402, 429 and other 5xx keep the normal handling (review finding, regression-tested).
    - **Credential kinds**: The handler excludes OAuth credentials from local selection through `coreauth.WithExcludedAuthKind` (new in `sdk/cliproxy/auth/conductor_selection.go`); ChatGPT OAuth and SIWC are not known to serve the Decisions API. In Home mode Home selects credentials and does not see this flag; an OAuth credential that arrives gets a request-scoped `501` without a cooldown. When no API-key credential of those providers serves the model, the client gets `400 model_not_supported` with an explanation. Using `gpt-6-luna` decisions therefore needs a `codex-api-key` or an `openai-compatibility` entry pointing at `https://api.openai.com/v1`.
    - **Related Upstream Work**: Issue [#6586](https://github.com/router-for-me/CLIProxyAPI/issues/6586) (open; asks the same, leaves OAuth support open). `patch/systemone-endpoint` is the same pattern for TypeSafe.
    - **Retire when**: Upstream serves `/v1/decisions`.

24. **`patch/codex-app-keepalive`**
    - **Goal**: Keep official Codex clients from dropping long silent `/v1/responses` streams (`idle timeout waiting for SSE`).
    - **Scope**: Carries PR #6348. For official Codex clients (`User-Agent`/`Originator`), the Responses stream forwarder writes `data: {"type":"codex.client.keepalive"}` after `streaming.codex-app-keepalive-seconds` (default 30) of upstream silence; `streaming.disable-codex-app-keepalive` turns it off. codex-rs resets its idle timer on every SSE data event and logs unknown types at debug, while SSE comments produce no event and do not reset it. The fork adapts the PR to the new `Flush`/`ChunkDone` stream options, reads the hot-reloaded config, writes nothing after a terminal event (`skipKeepAliveAfterTerminal`), makes the integration test race-free, and documents both settings.
    - **Known limits**: Covers silence after the first upstream event only. Before the first token, `send-keepalive-before-ft` still writes comments, which Codex ignores. When `keepalive-seconds` is longer than the application interval, the comment heartbeat follows the shorter application cadence.
    - **Related Upstream Work**: PR [#6348](https://github.com/router-for-me/CLIProxyAPI/pull/6348) (open, unreviewed), Issue [#6554](https://github.com/router-for-me/CLIProxyAPI/issues/6554) (closed; added `send-keepalive-before-ft`), PR [#4593](https://github.com/router-for-me/CLIProxyAPI/pull/4593)/[#4691](https://github.com/router-for-me/CLIProxyAPI/pull/4691) (open; turn upstream keepalive events into comments).
    - **Retire when**: PR #6348 or an equivalent lands.

25. **`patch/runtime-gc-tuning`**
    - **Goal**: Spend less CPU in garbage collection under load.
    - **Scope**: `Dockerfile` sets `ENV GOGC=200`. Request and response translation allocate several times the payload size per request (about 3 MiB for a 53 KiB chat request) while the live heap stays small, so with `GOGC=100` the collector runs every few requests; `GOGC=200` gave about 30% more throughput in an end-to-end Codex streaming benchmark (`-cpu 8`, local mock upstream). Cost: peak heap is up to three times the live heap instead of two. An explicit `GOGC` on the container wins; pin `GOMEMLIMIT` (for example 80% of the container limit) when memory is capped. Binary deployments get the same effect by exporting `GOGC=200`.
    - **Retire when**: Upstream tunes the collector in the image or in `cmd/server`.

26. **`patch/summary-config-single-scan`**
    - **Goal**: Stop rescanning the whole request body for every candidate summary path.
    - **Scope**: `internal/thinking/summary.go` indexes the top-level members once (`summaryDocument`, built with `util.ParseGJSONBytesNoCopy`) and resolves the up to 22 candidate paths of `ExtractSummaryConfig` / `ExtractExplicitSummaryConfig` against the member values instead of calling `gjson.GetBytes` on the full body each time. Same answers, including first-duplicate-wins (`summary_document_test.go` pins this against gjson). 124 µs → 25 µs per call on a 52 KiB chat body; the executor and translator registry call it several times per request.
    - **Related Upstream Work**: PR [#4684](https://github.com/router-for-me/CLIProxyAPI/pull/4684) introduced the readers; no upstream performance work on them was found.
    - **Retire when**: Upstream reads summary intent in a single pass.

27. **`patch/management-key-bcrypt-cache`**
    - **Goal**: Stop paying bcrypt on every management request.
    - **Scope**: `AuthenticateManagementKey` keeps the SHA-256 of the last key that passed `bcrypt.CompareHashAndPassword`, together with the secret hash it matched (`verifiedKey`, an `atomic.Pointer`). A repeat of the same key against the same hash is accepted after two constant-time compares (~2 µs instead of ~60 ms at the default cost); a different key or a changed hash goes through bcrypt again, and a failed bcrypt never populates the cache. Only the digest is kept. The IP ban counters are untouched. bcrypt was 5% of the 2026-10-15 profile, likely inflated by an open panel polling during collection.
    - **Retire when**: Upstream caches the verification or moves the panel to session tokens.

28. **`patch/pgo`**
    - **Goal**: Profile-guided optimisation of the release binary.
    - **Scope**: `cmd/server/default.pgo`, picked up by the default `-pgo=auto` of `go build` (the `Dockerfile` passes no `-pgo`). The current file merges six 300 s CPU profiles collected on 2026-10-16 from a production instance (`observability.pprof` on `127.0.0.1:8316`; 39.8 s of samples): the 11:36 set (Claude-heavy HTTP `/v1/responses` plus OpenAI chat completions, the Claude executor stream path about 30% of its 23.4 s) and the 21:09 set (Codex clients over WebSocket `/v1/responses`, about 28% of its 16.4 s). Neither set alone covers both executor paths, so keep merging sets with different traffic mixes rather than replacing one with the other. The 11:36 profiles are the ones in which `patch/responses-tool-identity-lite` was found; they replaced the 2026-10-15 profile (45 s of samples), which was inflated by the bcrypt hot spot patch 27 removed and the per-chunk request validation that upstream has since fixed (Issue [#6665](https://github.com/router-for-me/CLIProxyAPI/issues/6665)). Functions absent from the profile compile as before, so an unrepresentative profile cannot regress them; it only leaves gains on the table for the translators it did not see (Gemini). Refresh the file after re-collecting at peak with every client type active, merging in earlier sets whose traffic the new one lacks:
      ```bash
      for i in 1 2 3; do curl -s "http://127.0.0.1:8316/debug/pprof/profile?seconds=300" -o cpu-$i.pprof; sleep 600; done
      go tool pprof -proto <old-set>/cpu-*.pprof cpu-1.pprof cpu-2.pprof cpu-3.pprof > cmd/server/default.pgo
      ```
    - **Retire when**: Upstream ships a `default.pgo`.

29. **`patch/quota-api`**
    - **Goal**: Let a client API key read, for the OAuth credentials selected for it, the upstream quota data the management panel shows.
    - **Scope**: `GetClientUsage` (`internal/api/handlers/management/client_usage.go`) is registered on the `/v1` group behind the normal client-key auth. The caller's key (`userApiKey`, the raw config key) sees only the credentials listed for it in `observability.quota-api.keys` (`{api-key, credentials: [auth IDs]}`, repeated keys merged); a key without an entry, or whose IDs match nothing, gets `{"credentials":[]}`. For each visible enabled OAuth credential of `codex`, `claude`, `antigravity` and `xai` it calls the panel's upstream quota endpoints with the panel's headers (`client_usage_providers.go`; xAI both the weekly credits and monthly billing views, Antigravity its endpoints in order until one succeeds), reusing api-call token resolution and proxy transport. Each credential carries `{name, provider, status, error, observed_at, responses:[{endpoint, status_code, body, error}]}`: `body` is the upstream JSON as returned, whatever the status, with `email`, `user_id`/`userId` and `account_id`/`accountId` removed at any depth and in any letter case, and `name` is the credential's note, else its provider. Results are cached per credential for `cache-ttl-seconds` (default 300, minimum 30, failures included), concurrent probes of a credential are collapsed with singleflight, at most 4 probes run at once across all requests, and the cache is pruned against the whole pool. `quota-api.enabled` is ignored but still accepted so older configs validate. The selection lives under `quota-api` rather than on `access.api-keys` entries because `patch/client-api-key-models` rewrites those entries on save (core `foldClientKeysIntoAPIKeys`, panel `serializeClientApiKeyItem`) and would drop an unknown field; there is no dependency on that patch. An earlier Sub2API-compatible envelope for CC Switch and Minis was dropped on 2026-10-16.
    - **Known limits**: A selected key sees every field of the upstream bodies other than the redacted IDs (plan, credits, organisation names). Credentials without a note share their provider name. A renamed or deleted auth file leaves a stale ID that matches nothing until it is cleared. Paid xAI accounts without billing data report an error (the panel's chat ping is not replicated). Kimi, Meta, Devin and plugin quotas are not covered. Each probe is bounded by a 20 s timeout (listed in `AGENTS.md`).
    - **Cross-repo**: The panel patch `patch/quota-api` (stacked on panel `patch/client-key-provider-scope`) picks each key's credentials in the API keys dialog and edits the cache TTL. Without it, edit `observability.quota-api.keys` by hand.
    - **Related Upstream Work**: None found (searched quota endpoint, `v1/usage`, 额度, sub2api usage on 2026-10-16). Issue [#6519](https://github.com/router-for-me/CLIProxyAPI/issues/6519) concerns the management plugin quota contract only.
    - **Retire when**: Upstream exposes credential quota to client keys.

30. **`patch/responses-tool-identity-lite`**
    - **Goal**: Stop rendering Gemini function declarations when only the tool identity map is needed.
    - **Scope**: `util.ResponsesToolReverseIdentityMap` called `BuildGeminiFunctionDeclarations`, which also renders every declaration and runs `CleanJSONSchemaForGeminiJSONSchema` on each tool's parameters. `helps.ApplyPatchRequested` reaches it once per streamed request through `InitializeApplyPatchStream` on every executor except Codex, xAI and Meta, so Claude and OpenAI-compatible requests paid for schema cleaning they never used: 4.4% of CPU in the 2026-10-16 production profile. The shared resolution is now `buildGeminiFunctionDeclarations(root, withDeclarations)`; the identity map passes `false`, and the forward and reverse maps come from the same code either way. 41 schema-bearing tools: 2.93 ms → 88 µs, 45801 → 332 allocs per call. `responses_tools_identity_test.go` pins both maps against `BuildGeminiFunctionDeclarations` and carries the benchmark.
    - **Related Upstream Work**: None found (searched `ApplyPatchRequested`, `ResponsesToolReverseIdentityMap`, `BuildGeminiFunctionDeclarations`, apply_patch performance on 2026-10-16).
    - **Retire when**: Upstream stops rendering declarations for the identity map.

31. **`patch/commandcode-oauth`**
    - **Goal**: Log in to Command Code (commandcode.ai) from the panel like the other OAuth providers, and serve the saved credential.
    - **Scope**:
      - Login (`internal/auth/commandcode`, `RequestCommandCodeToken`): `GET /v8/management/oauth/auth-url?provider=commandcode` returns the Studio CLI page `https://commandcode.ai/studio/auth/cli?callback=http://127.0.0.1:54546/callback&state=…&mode=redirect`, the flow `cmd login` uses (read from `command-code@1.82.5`). Studio sends the API key itself (`apiKey`, plus `userId`, `userName`, `keyName`); there is no refresh token. It does not redirect with a query: it POSTs the fields to `/callback`, as a browser form (answered with a 303 to `/callback/complete`) or as a CORS fetch with a JSON body, so the address bar shows only the bare `/callback`. Web UI logins serve 54546 with `commandCodeCallbackHandler` (`auth_files_commandcode_callback.go`), which reads the form, JSON or legacy query, answers CORS and Private Network Access preflights, and writes the pending session's callback file; `docker-compose.yml` publishes 54546. When the browser cannot reach the server (remote host), the panel builds `http://127.0.0.1:54546/callback?apiKey=…&state=…` from the pasted payload or bare key and submits it to `POST /oauth/callback`, whose `apiKey` parameter is read only for Command Code sessions. The key is validated with `GET https://api.commandcode.ai/alpha/whoami` and saved as a `commandcode` auth file (`api_key`, `base_url`, `auth_kind: oauth`, email/user/org).
      - Serving: the synthesizer exposes `base_url` (default `https://api.commandcode.ai/provider/v1`) and `api_key` attributes, and keeps the credential OAuth-kind; `OpenAICompatExecutor("commandcode")` executes it. Models come from the public `/provider/v1/models` catalog, keeping only models served on `/chat/completions` or `/systemone` (Command Code serves Claude only on `/messages`), marked user-defined so reasoning settings pass through. Models served only on `/systemone` declare the `decisions` output modality, and the catalog, which does not list TypeSafe models (checked 2026-10-16), is completed with `typesafe/jev` unless it lists that ID itself; with `patch/systemone-endpoint` the credential then serves `POST /v1/systemone` (`<base_url>/systemone` answers, verified 2026-10-16), so no `openai-compatibility` copy of the key is needed. The catalog is cached for 30 minutes and refetched at the next model registration, keeping the last good list on failure and retrying no sooner than a minute later; the fetch is a metadata call bounded by a 30 s deadline, like patch 21's discovery. Model IDs follow the usual rules: published bare, with the auth file's `prefix` added as an alias when set, and prefixed only under `force-model-prefix`. Set a `prefix` on the auth file if unprefixed `gpt-*`, `kimi-*` or `grok-*` requests must not spend Command Code credits.
    - **Known limits**: Go plan credentials get `403 upgrade_required` (the Go plan has no Provider API access; the CLI's private `/alpha/generate` is not bridged). No CLI/TUI login flag. The built-in `typesafe/jev` is static: a renamed or withdrawn model fails at request time, and without `patch/systemone-endpoint` it is listed but cannot be served. A changed catalog appears at the next registration (restart or credential change). Do not combine with the store plugins `commandcode` or `commandcode-go-cliproxyapi`, which register the same provider name.
    - **Cross-repo**: The panel patch `patch/commandcode-oauth` adds the login tile, the manual payload/key input and a quota card reading `/alpha/billing/credits` and `/alpha/billing/subscriptions` through api-call.
    - **Related Upstream Work**: Issue [#3955](https://github.com/router-for-me/CLIProxyAPI/issues/3955) (closed), PR [#3969](https://github.com/router-for-me/CLIProxyAPI/pull/3969) (closed; the maintainer asked for a plugin instead of a built-in CLI wrapper), PR [#5060](https://github.com/router-for-me/CLIProxyAPI/pull/5060) (open, conflicting; Go plan through `/alpha/generate`, API keys only). The store plugins above take API keys from config and have no browser login. Searched `commandcode`, `command code`, `command-code` on 2026-10-16.
    - **Retire when**: Upstream ships a Command Code login, or the fork switches to a plugin that provides it.

32. **`patch/session-claude-identity-once`**
    - **Goal**: Stop re-scanning large request bodies for Claude metadata identities during session extraction.
    - **Scope**: `session.ExtractSessionInfo` already indexes the top-level members once (`newSessionObject`, upstream `1dffddf4`), but called `ClaudeMetadataIdentities(payload)` / `ClaudeMetadataParentSessionID(payload)` up to three times per request, each re-parsing the raw body and walking it for `metadata.user_id`, `request` and `contents`. A Codex Responses body carries no `metadata`, so every call paid full top-level scans over the `input` history: 0.30 s of the 4.54 s WebSocket-to-Codex path in the 2026-10-16 21:09 profile. Extraction now resolves the identities lazily, at most once, from the existing `sessionObject` index (`claudeMetadataIdentitiesFromSessionObject`); the exported helpers keep their signatures and results and share the user_id parsing (`parseClaudeMetadataIdentities`). 1 MiB Codex body: 710 → 148 µs per extraction; 1 MiB Claude body with `metadata.user_id`: 142 → 148 µs, 9 → 7 allocs. Output is identical for valid JSON; malformed JSON may recover differently, as the rest of the function already does since `1dffddf4`. `info_equivalence_test.go` pins the output against the pre-change logic; `info_performance_test.go` carries the benchmark.
    - **Related Upstream Work**: Issue [#6114](https://github.com/router-for-me/CLIProxyAPI/issues/6114) (closed; session extraction rescanning bodies pinned CPA at 27–31 cores), fixed in part by `1dffddf4`. PR [#6058](https://github.com/router-for-me/CLIProxyAPI/pull/6058) (open, conflicting, no review) indexes fields more broadly and also reuses Claude identity parsing within an extraction; it predates `1dffddf4`. This patch carries only the identity part on top of current upstream. Searched `ClaudeMetadataIdentities`, `ExtractSessionInfo`, session/WebSocket performance on 2026-10-16.
    - **Retire when**: Upstream resolves Claude identities once per extraction (e.g. by merging #6058 or an equivalent).

## Measured and Rejected

- **Tighter title-helper pre-filter** (2026-10-16). `isClaudeTitleHelperRequest` was 5.7% of the 2026-10-16 profile because upstream's `jsonMayContainASCII` (Issue [#6291](https://github.com/router-for-me/CLIProxyAPI/issues/6291), commit `5dbce4f3`) passes almost every Claude Code request: ordinary turns carry `output_config` (effort) and Go-escaped `\u003c`/`\u0026`. A needle-aware escape check that rejected those turns was exact (fuzzed against the reference walk) but slower: proving a needle absent needs a full scan of the body, which costs as much as the gjson walk it skips (3 MB Codex-to-Claude body: 2.2 ms → 2.8 ms per call). Indexing the top-level members once would save only one skim per call. The remaining lever is issue #6291's second suggestion, classifying the request once per `ExecuteStream`, which touches many executor call sites.
- **Faster JSON validator on the Responses WebSocket path** (2026-10-16). `encoding/json.Valid` was 0.44 s of the 4.54 s WebSocket-to-Codex path in the 21:09 profile: `normalizeResponsesWebsocketPassthroughRequest` (the only JSON gate for an incoming request; invalid JSON answers 400) and `websocketJSONPayloadsFromChunk` (drops invalid upstream lines, O(chunk), not O(request) like Issue #6665). Neither check is redundant, so the only lever is a faster validator. `gjson.ValidBytes` (v1.18.0, Go 1.27) is 5% faster on a 1 MiB Codex request (488 → 463 µs) and a 64 KiB `response.completed` event (32.6 → 30.9 µs), 25% on a 174-byte delta (123 → 92 ns), both allocation-free; that saves about 0.03 s per 15 minutes of this traffic. It also diverges on deep nesting: `encoding/json` rejects 20000 nested arrays, gjson accepts them through unbounded recursion, which weakens the gate on client input. Not worth it.

## Combination Conflicts

Textual conflicts between two patches that neither branch owns are resolved at the step where they occur, in `.github/workflows/upstream-sync.yml`, and the same commands appear in the rebuild procedure below.

- **`patch/client-api-key-models` + `patch/client-key-provider-scope`**, `sdk/api/handlers/handlers_interceptors.go`: both insert a filter at the same spot in `WriteModelListResponse`, after response interceptors. `.github/scripts/resolve-union-conflict.sh` keeps both blocks (client model rules first, then key scopes), so a model is listed only when both allow it. It checks the SHA-256 of each side and fails closed on any other shape: when either branch changes that block, review the combination again and update the hashes. `sdk/api/handlers/downstream_client_access_combination_test.go` (owned by `ci`) checks the intersection in lists and execution. Retire with either patch.

## Retired Patch Branches

- `patch/p2-runtime-optimizations` was split into patches 3–8 on 2026-10-14.
- `patch/p0-oauth-and-executors` was split into patches 11–13 on 2026-10-14. Its multi-line Codex SSE assembly was dropped because upstream implemented `CodexSSEScanner` (Issue [#6575](https://github.com/router-for-me/CLIProxyAPI/issues/6575)).
- `patch/claude-responses-request-validate-once` was retired on 2026-10-16: upstream commit `7a511e4d` (Issue [#6665](https://github.com/router-for-me/CLIProxyAPI/issues/6665)) pins the validated request body per stream with the same change, and the patch's own test redeclared upstream's new benchmark.

## Rebuild & Validation Procedure

```bash
git fetch upstream main --tags
branches=(patch/http-compression patch/responses-ws-sse-incremental patch/chatgpt-utls-pool patch/runtime-memory-bounds patch/startup-model-registration patch/claude-api-key-policy patch/management-panel-update patch/management-ua-shim patch/downstream-panel-source patch/fix-interrupt-test-race patch/claude-thinking-off patch/claude-search-recovery patch/codex-oauth-resilience patch/embeddings-and-rerank patch/client-api-key-models patch/proxy-transport-reuse patch/systemone-endpoint patch/model-capabilities patch/quota-cooldown-fail-fast patch/responses-input-tokens patch/openai-compat-model-discovery patch/client-key-provider-scope patch/openai-decisions-endpoint patch/codex-app-keepalive patch/runtime-gc-tuning patch/summary-config-single-scan patch/management-key-bcrypt-cache patch/pgo patch/quota-api patch/responses-tool-identity-lite patch/commandcode-oauth patch/session-claude-identity-once)
# Name every ref: the default origin refspec may only track main.
git fetch origin ci "${branches[@]}"

git checkout -B main upstream/main
git merge --squash origin/ci || git restore --source=origin/ci --staged --worktree -- README.md
git commit -m "merge ci branch"
for branch in "${branches[@]}"; do
  if ! git merge --squash "origin/${branch}"; then
    case "${branch}" in
      # See "Combination Conflicts".
      patch/client-key-provider-scope)
        bash .github/scripts/resolve-union-conflict.sh sdk/api/handlers/handlers_interceptors.go \
          f42a0e3014a41933a4ba6c380af390c6a42d3a4513aebb944cbc0addf15d6601 \
          22d0e60853c89ef17420ad5709ddd666de3c70edbf2479f570b062539af28354 || exit 1 ;;
      *) echo "conflict in ${branch}: fix it on the patch branch"; exit 1 ;;
    esac
  fi
  git diff --cached --quiet && { echo "${branch} is empty: check whether upstream absorbed it"; exit 1; }
  git commit -m "merge ${branch} branch"
done

# Must match the "Validate integrated build and patch tests" step in
# .github/workflows/upstream-sync.yml. The full ./internal/runtime/executor/...
# package is not run here: it has long-hanging tests, so it is covered by
# targeted -run patterns instead. ./sdk/cliproxy is also targeted:
# TestPutOpenAICompatConcurrentWithAuthFieldsPatch_Issue6559 fails on
# upstream/main itself (seen 2026-10-15).
go build -o test-output ./cmd/server && rm -f test-output
go test ./sdk/api/handlers/... ./internal/api/middleware/... ./internal/api/handlers/management/... ./internal/translator/claude/openai/responses/... ./internal/config/... ./internal/watcher/diff/... ./internal/runtime/executor/helps/... ./internal/managementasset/... ./internal/thinking/... ./internal/registry/... ./internal/auth/codex/... ./internal/auth/commandcode/... ./sdk/proxyutil/... ./internal/client/codex/models/... ./sdk/cliproxy/auth/... ./internal/client/codex/live/... ./internal/redisqueue/... ./sdk/cliproxy/usage/... ./internal/util/...
go test -run "TestServiceRun_RegistersLoadedAuthModelsBeforeListening|TestModelCapabilities|TestBuildOpenAICompatibilityConfigModels|TestFetchOpenAICompatibilityModelIDs|TestMergeDiscoveredCompatModels|TestCompatModelsWithDiscovery|TestDiscoverCompatModelIDs|TestCompatDiscoveryCacheKey|CommandCode" ./sdk/cliproxy
go test -run "CommandCode" ./internal/watcher/synthesizer
go test -run "TestCodexExecutorUpstreamRequestCompression|TestCodexAutoExecutorAllowsHTTPDownstreamToUseWebsocketUpstream|TestCollectCodexOutputItemDone|TestCodexOutputItemPatchBuffer|TestOfficialAPIKeyWithEmptyOrAutoCloakConfigDoesNotCloak|TestClassifyClaudeUpstreamErrorPromotesLowCreditBalance400To402|TestCodexTemporaryIPRestrictionClassifiedAs503|TestGeminiExecutor_Embeddings|TestOpenAICompatExecutor_Embeddings|TestOpenAICompatExecutor_Rerank|TestOpenAICompatExecutorSystemOne|TestOpenAICompatExecutorDecisions|TestCodexExecutorDecisions|TestOpenAIDecisionsErrorScope" ./internal/runtime/executor
go test -run "TestSystemOneRoute|TestDecisionsRouteRegistered|TestResponsesInputTokensRoutesRegistered|KeyScope|CodexOnly" ./internal/api
git push --force-with-lease origin HEAD:main
```

## CI/CD Workflows Owned by `ci`

- `.github/workflows/upstream-sync.yml`: Runs daily at `20:18 UTC` (`04:18 Asia/Shanghai`) and via `workflow_dispatch`. Rebuilds `main` from `upstream/main`, fails on a conflicting or empty patch, validates the candidate, and pushes with `--force-with-lease` only when the rebuilt tree differs from `main`.
- `.github/workflows/ghcr-docker.yml`: Builds and publishes multi-arch (`linux/amd64`, `linux/arm64`) images to `ghcr.io/johnsonran/cli-proxy-api` on `main` pushes, `v*` tags, `workflow_dispatch`, and `Upstream Sync` runs that actually moved `main`.

## Downstream Policy

- **No upstream pull requests.** This fork never opens pull requests against the upstream repository; the `Related Upstream Work` entries record upstream context only. Patches are maintained downstream indefinitely and are retired only when upstream lands an equivalent implementation, detected by a rebuild that finds the patch empty or conflicting.
- **`main` is intentionally unprotected.** The scheduled sync rebuilds and force-pushes `main`, so it carries no branch protection. Safety comes from `--force-with-lease`, the `upstream-sync-main` concurrency group, fail-closed conflict and empty-patch handling, and validation before the push. Protect the `patch/*` and `ci` branches against deletion instead; `main` is a derived artifact that nothing else may be based on.
- **Cross-repo patches.** Core and panel patches that depend on each other are listed under `Cross-repo` on both sides. Ship the provider side first.
