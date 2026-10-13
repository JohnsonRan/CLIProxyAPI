# Downstream Fork Maintenance Guide (`JohnsonRan/CLIProxyAPI`)

This repository is a maintained downstream distribution of [`router-for-me/CLIProxyAPI`](https://github.com/router-for-me/CLIProxyAPI). It tracks `upstream/main` and layers independently reviewable, buildable, and testable patches on top of the latest upstream baseline.

## Branch Architecture

- **Upstream baseline**: `https://github.com/router-for-me/CLIProxyAPI.git` (`main`)
- **`ci`**: Downstream maintenance overlay carrying this `MAINTAIN.md`, downstream `README.md` notice, `.github/workflows/upstream-sync.yml`, and `.github/workflows/ghcr-docker.yml`. Never mix product runtime changes into `ci`.
- **`patch/<name>`**: Independent product patch branches, each branched directly from `upstream/main` and kept suitable for standalone review or upstream PR submission.
- **`main`**: Integrated release branch rebuilt from `upstream/main` with one countable squash commit per branch (`merge ci branch` followed by each `merge patch/<name> branch` in fixed order). Never commit fixes only to `main`.

## Active Patch Branches (Fixed Integration Order)

1. **`patch/http-compression`**
   - **Goal**: End-to-end HTTP, SSE, and WebSocket compression (`gzip`, `br`, `zstd`, `deflate`) across both downstream and upstream legs to eliminate wasted bandwidth.
   - **Scope**:
     - Downstream request body decompression (`sdk/api/handlers/request_body.go`, `internal/api/middleware/request_logging.go`, and all API handlers including Claude `/v1/messages`, `/v1/messages/count_tokens`, Gemini `/v1beta/*`, and `/v1/alpha/search`) supporting `gzip`, `br`, `zstd`, `deflate`, multi-coding chains, and magic-byte `gzip`/`zstd` detection with `max-request-body-bytes` protection.
     - Downstream HTTP & SSE response compression middleware (`internal/api/middleware/response_compression.go`) negotiating `zstd > br > gzip > deflate` via `Accept-Encoding` (`q=` weights) and flushing stream encoders on every SSE `Flush()` / `FlushError()` call, plus `EnableCompression: true` on `wsrelay` and `codex/live` WebSocket upgraders/dialers.
     - Upstream HTTP response decompression (`internal/runtime/executor/helps/response_compression.go`) across `codex`, `openai-compatibility`, `gemini`, `antigravity`, `kimi`, `xai`, and `meta` executors (`Accept-Encoding: gzip, deflate, br, zstd` + transparent `gzip`/`deflate`/`br`/`zstd`/magic-byte decoding).
     - Upstream request body compression (`internal/runtime/executor/helps/codex_upstream_compression.go`, `upstream.codex.upstream-request-compression`) defaulting to `zstd` (level 3, matching official Codex CLI) for `chatgpt.com` `/responses` and `/responses/compact` bodies `>= 1024` bytes, also supporting `gzip`, `br`, and `off`.
   - **Related Upstream Work**: PR [#5132](https://github.com/router-for-me/CLIProxyAPI/pull/5132), PR [#4461](https://github.com/router-for-me/CLIProxyAPI/pull/4461).

2. **`patch/responses-ws-sse-incremental`**
   - **Goal**: Complete HTTP Incremental SSE, Standard/Incremental WebSocket, and HTTP-to-WebSocket upstream transport bridging.
   - **Scope**:
     - Enable `permessage-deflate` compression (`EnableCompression: true`) on `/v1/responses` WebSocket upgrader (`sdk/api/handlers/openai/openai_responses_websocket.go`).
     - Standard & incremental WebSocket normalization (`sdk/api/handlers/openai/openai_responses_websocket_requests.go`): default omitted `"type"` on valid request objects to `"response.create"`, coerce string `"input"` into a user message array item, inherit top-level `tools`, `tool_choice`, and `parallel_tool_calls` across incremental turns, and preserve prewarm `additional_tools` across transcript replacements and compaction replays.
     - Stateful HTTP Incremental SSE & non-stream continuation (`sdk/api/handlers/openai/openai_responses_http_incremental.go`): bounded TTL cache (`30m`, max `1024` entries) keyed by `response.id` on `POST /v1/responses` so clients sending incremental `previous_response_id` + delta `input` over HTTP/SSE automatically expand history and inherit tools/instructions/auth affinity when targeting stateless HTTP upstreams.
     - HTTP/SSE-to-WebSocket upstream bridge (`oauth.providers.codex.enable-websocket-upstream` / per-auth `websockets: true` in `internal/runtime/executor/codex_websockets_executor.go`): allow HTTP/SSE downstream clients (e.g., ZCode, Claude Code, OpenCode) to use Codex WebSocket upstream while keeping `/responses/compact` and `/v1/images/*` on HTTP and preserving 426 fallback.
   - **Related Upstream Work**: PR [#4907](https://github.com/router-for-me/CLIProxyAPI/pull/4907), PR [#3562](https://github.com/router-for-me/CLIProxyAPI/pull/3562), PR [#5420](https://github.com/router-for-me/CLIProxyAPI/pull/5420), Issue [#2596](https://github.com/router-for-me/CLIProxyAPI/issues/2596).

3. **`patch/p2-runtime-optimizations`**
   - **Goal**: Connection reuse, transient handshake resilience, bounded memory retention, and startup model readiness.
   - **Scope**:
     - Pool and reuse ChatGPT uTLS HTTP/2 connections (`cachedChatGPTRoundTripper` + `h2Pool` + TLS 1.3 `ClientSessionCache`) and retry transient dial/handshake failures up to 3 times before sending request bytes (`internal/runtime/executor/helps/utls_client.go`).
     - Expire idle WebSocket tool-call and tool-output replay caches after 1 hour instead of passing `ttl = 0` (`sdk/api/handlers/openai/openai_responses_websocket_toolcall_repair.go`).
     - Cap Codex streaming `response.output` reconstruction buffer (`codexOutputItemPatchBuffer`) at 16 MiB (`internal/runtime/executor/codex_executor_terminal.go`, `codex_executor_stream.go`).
     - Register loaded auth models (`syncPluginModelRuntime`) in `Service.Run` before the HTTP listener opens so early startup requests do not fail with `model_not_found` (`sdk/cliproxy/service_lifecycle.go`).
     - Inject a browser-version-invariant `navigator.userAgent` stabilizer and automatic `enc::v1::` `localStorage` migration shim when serving `/management.html` (`internal/managementasset/updater.go`, `internal/api/server_management.go`) while keeping the on-disk asset byte-identical for SHA256 digest checks.
     - Keep official Anthropic API keys (`sk-ant-usr-...` / `sk-ant-api03-...`) uncloaked when saved with default/empty or `auto` cloak blocks in `resolveClaudeWirePolicy`, and promote `400 "Your credit balance is too low..."` responses to credential-scoped `402 Payment Required` cooldowns (`internal/runtime/executor/claude_executor_cloaking.go`, `claude_executor_request.go`).
   - **Related Upstream Work**: PR [#5261](https://github.com/router-for-me/CLIProxyAPI/pull/5261), PR [#5804](https://github.com/router-for-me/CLIProxyAPI/pull/5804), PR [#6398](https://github.com/router-for-me/CLIProxyAPI/pull/6398), PR [#6461](https://github.com/router-for-me/CLIProxyAPI/pull/6461), PR [#6464](https://github.com/router-for-me/CLIProxyAPI/pull/6464).

4. **`patch/p0-oauth-and-executors`**
   - **Goal**: Claude & Codex OAuth refresh resilience, Claude 5.5 / Fable official thinking-off compliance, Claude encrypted `web_search` failover recovery, and Codex multi-line SSE framing.
   - **Scope**:
     - Send `thinking: {"type": "between_tools"}` when turning thinking off on `claude-sonnet-5-5`, fall back to `adaptive` (`low`, `display: "omitted"`) on adaptive-only models (`claude-opus-5-5`, `claude-fable-5`, `claude-fable-5-1`), and resolve dated (`-YYYYMMDD`) / bracketed (`[1m]`) Claude variants in `LookupStaticModelInfo` (`internal/thinking/`, `internal/registry/`, `internal/modelconfig/`).
     - Assemble multi-line `data:` Codex SSE events before JSON parsing (`internal/runtime/executor/helps/codex_sse_scanner.go`, `internal/runtime/executor/codex_executor_execute.go`, `codex_executor_stream.go`).
     - Recover Claude conversations after OAuth credential failover when an earlier turn's native `web_search_tool_result` `encrypted_content` is rejected with HTTP 400 (`internal/runtime/executor/helps/claude_search_recovery.go`, `internal/runtime/executor/claude_executor_execute.go`, `claude_executor_stream.go`).
     - Classify Codex `401 Your IP is not authorized to make this request` as `503` (`ip_temporarily_restricted`, 30s retry hint) so proxy IP blocks do not trigger OAuth token refresh (`internal/runtime/executor/codex_executor_terminal.go`, `codex_websockets_errors.go`).
     - Cache completed Codex OAuth token refreshes for 5 minutes for late concurrent callers, stop retrying terminal `invalid_grant`/`refresh_token_invalidated` errors within `RefreshTokensWithRetry`, and send the official Codex `User-Agent` on OAuth exchange/refresh (`internal/auth/codex/openai_auth.go`).
   - **Related Upstream Work**: PR [#6571](https://github.com/router-for-me/CLIProxyAPI/pull/6571), PR [#6570](https://github.com/router-for-me/CLIProxyAPI/pull/6570), PR [#6576](https://github.com/router-for-me/CLIProxyAPI/pull/6576), PR [#6447](https://github.com/router-for-me/CLIProxyAPI/pull/6447), PR [#6437](https://github.com/router-for-me/CLIProxyAPI/pull/6437), PR [#6165](https://github.com/router-for-me/CLIProxyAPI/pull/6165), PR [#5766](https://github.com/router-for-me/CLIProxyAPI/pull/5766), PR [#5850](https://github.com/router-for-me/CLIProxyAPI/pull/5850).

5. **`patch/embeddings-and-rerank`**
   - **Goal**: Add standard OpenAI-compatible text embedding (`/v1/embeddings`, `/v1/embedding`) and reranking (`/v1/rerank`, `/rerank`) endpoints for OpenAI-compatible and Gemini providers.
   - **Scope**:
     - Declare optional `embedding: true` and `rerank: true` flags in `OpenAICompatibilityModel` (`internal/config/config_types.go`, `config.example.yaml`).
     - Mount `/v1/embeddings`, `/v1/embedding`, `/v1/rerank`, and top-level `/rerank` routes in `internal/api/server_routes.go`.
     - Implement `Embeddings` and `Rerank` request validation and non-streaming execution in `OpenAIAPIHandler` (`sdk/api/handlers/openai/openai_handlers.go`).
     - Route embedding and rerank requests in `OpenAICompatExecutor` (`internal/runtime/executor/openai_compat_executor.go`) to upstream `${baseURL}/embeddings` and `${baseURL}/rerank`.
     - Publish successful rerank calls to the usage queue, reading OpenAI `usage` or `meta.tokens` / `meta.billed_units`.
     - Translate OpenAI `/v1/embeddings` requests to Google Generative Language `:embedContent` (single) and `:batchEmbedContents` (batch) in `GeminiExecutor` (`internal/runtime/executor/gemini_executor.go`, `internal/runtime/executor/helps/gemini_embedding.go`).
   - **Related Upstream Work**: Issue [#5961](https://github.com/router-for-me/CLIProxyAPI/issues/5961), Issue [#3641](https://github.com/router-for-me/CLIProxyAPI/issues/3641), Issue [#1084](https://github.com/router-for-me/CLIProxyAPI/issues/1084), PR [#1191](https://github.com/router-for-me/CLIProxyAPI/pull/1191).

6. **`patch/client-api-key-models`**
   - **Goal**: Configure native (non-plugin) visible and usable model access control globally and per client API key.
   - **Scope**:
     - Add `ClientKeyConfig` (`api-key`, `models`, `allowed-models`, `excluded-models`), `ClientModels`, and `ClientExcludedModels` to `SDKConfig` (`internal/config/sdk_config.go`, `client_models.go`, `config_v8.go`, `config_load.go`, `parse.go`, `sdk/config/config.go`).
     - Support global default models under `access.models` / `access.allowed-models` / `access.excluded-models` and per-key overrides/wildcards in `access.api-keys` (both scalar string and mapping entries).
     - Filter model lists in `WriteModelListResponse` (`/v1/models` across OpenAI, Codex client catalog, Claude, Grok, and Gemini formats) and model detail endpoints (`/v1/models/*model`, `/v1beta/models/*action`), returning 404 for unauthorized models.
     - Enforce model usability authorization in `executeWithAuthManagerFormats`, `executeStreamWithAuthManagerFormats`, `executeCountWithAuthManager`, `ExecuteProtocolWithAuthManager`, and Codex Realtime (`/v1/realtime`, `/v1/realtime/calls`, `/v1/realtime/client_secrets`), returning 403 Forbidden with `code: model_not_allowed` for unauthorized models.
     - Document usage in `config.example.yaml` and add unit and integration tests.
     - Expose `GET /v8/management/models` (`GetModelCatalog`) so management clients can list servable models without client-key filtering.
   - **Related Upstream Work**: PR [#6568](https://github.com/router-for-me/CLIProxyAPI/pull/6568), PR [#4030](https://github.com/router-for-me/CLIProxyAPI/pull/4030), PR [#5517](https://github.com/router-for-me/CLIProxyAPI/pull/5517).

## Rebuild & Validation Procedure

```bash
git fetch upstream main --tags
git fetch origin ci patch/http-compression patch/responses-ws-sse-incremental patch/p2-runtime-optimizations patch/p0-oauth-and-executors patch/embeddings-and-rerank patch/client-api-key-models

git checkout -B main upstream/main

git merge --squash origin/ci
git commit -m "merge ci branch"

git merge --squash origin/patch/http-compression
git commit -m "merge patch/http-compression branch"

git merge --squash origin/patch/responses-ws-sse-incremental
git commit -m "merge patch/responses-ws-sse-incremental branch"

git merge --squash origin/patch/p2-runtime-optimizations
git commit -m "merge patch/p2-runtime-optimizations branch"

git merge --squash origin/patch/p0-oauth-and-executors
git commit -m "merge patch/p0-oauth-and-executors branch"

git merge --squash origin/patch/embeddings-and-rerank
git commit -m "merge patch/embeddings-and-rerank branch"

git merge --squash origin/patch/client-api-key-models
git commit -m "merge patch/client-api-key-models branch"

go build -o test-output ./cmd/server && rm -f test-output
go test ./sdk/api/handlers/... ./internal/api/... ./internal/config/... ./internal/watcher/diff/... ./internal/runtime/executor/... ./internal/thinking/... ./internal/auth/codex/...
git push --force-with-lease origin HEAD:main
```

## CI/CD Workflows Owned by `ci`

- `.github/workflows/upstream-sync.yml`: Runs daily at `20:18 UTC` (`04:18 Asia/Shanghai`) and via `workflow_dispatch` to rebuild `main` from `upstream/main`, validate the integrated candidate, and push safely with `--force-with-lease`.
- `.github/workflows/ghcr-docker.yml`: Builds and publishes multi-arch (`linux/amd64`, `linux/arm64`) Docker images to `ghcr.io/johnsonran/cli-proxy-api` on `main` pushes, `v*` tags, successful `Upstream Sync` runs, and `workflow_dispatch`.
