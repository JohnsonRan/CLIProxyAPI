# Downstream Fork Maintenance Guide (`JohnsonRan/CLIProxyAPI`)

This repository is a maintained downstream distribution of [`router-for-me/CLIProxyAPI`](https://github.com/router-for-me/CLIProxyAPI). It tracks `upstream/main` and layers independently reviewable, buildable, and testable patches on top of the latest upstream baseline.

## Branch Architecture

- **Upstream baseline**: `https://github.com/router-for-me/CLIProxyAPI.git` (`main`)
- **`ci`**: Downstream maintenance overlay carrying this `MAINTAIN.md`, downstream `README.md` notice, `.github/workflows/upstream-sync.yml`, and `.github/workflows/ghcr-docker.yml`. Never mix product runtime changes into `ci`.
- **`patch/<name>`**: Independent product patch branches, each branched directly from `upstream/main` and kept suitable for standalone review and independent build/test. One patch carries one feature or fix, so a patch becomes empty once upstream absorbs it.
- **`main`**: Integrated release branch rebuilt from `upstream/main` with one countable squash commit per branch (`merge ci branch` followed by each `merge patch/<name> branch` in fixed order). Never commit fixes only to `main`.

The integration order lives in `PATCH_BRANCHES` in `.github/workflows/upstream-sync.yml`. Keep it identical to the list below.

## Active Patch Branches (Fixed Integration Order)

1. **`patch/http-compression`**
   - **Goal**: End-to-end HTTP, SSE, and WebSocket compression (`gzip`, `br`, `zstd`, `deflate`) across both downstream and upstream legs.
   - **Scope**:
     - Downstream request body decompression (`sdk/api/handlers/request_body.go`, `internal/api/middleware/request_logging.go`, and the Claude, Gemini and `/v1/alpha/search` handlers) supporting `gzip`, `br`, `zstd`, `deflate`, multi-coding chains, and magic-byte `gzip`/`zstd` detection with `max-request-body-bytes` protection.
     - Downstream HTTP & SSE response compression middleware (`internal/api/middleware/response_compression.go`) negotiating `zstd > br > gzip > deflate`, flushing stream encoders on every SSE flush, plus `EnableCompression: true` on `wsrelay` and `codex/live` WebSocket upgraders/dialers.
     - Upstream HTTP response decompression (`internal/runtime/executor/helps/response_compression.go`) across `codex`, `openai-compatibility`, `gemini`, `antigravity`, `kimi`, `xai`, and `meta` executors.
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
   - **Scope**: `internal/runtime/executor/helps/utls_client.go` pools one HTTP/2 connection per host and proxy with a TLS session cache, retries dial/handshake failures up to 3 times before any request byte is sent, retries a pooled request on a new connection only when it was never sent (unusable connection or `REFUSED_STREAM`, body rewound through `GetBody`), retires replaced or failed connections gracefully, and closes idle pooled connections after 90 seconds.
   - **Related Upstream Work**: PR [#5261](https://github.com/router-for-me/CLIProxyAPI/pull/5261), PR [#6398](https://github.com/router-for-me/CLIProxyAPI/pull/6398).

4. **`patch/runtime-memory-bounds`**
   - **Goal**: Bound long-lived memory.
   - **Scope**: Expire idle WebSocket tool-call and tool-output replay sessions after 1 hour (`openai_responses_websocket_toolcall_repair.go`), and stop retaining Codex `response.output_item.done` items for `response.completed` reconstruction past 16 MiB (`codex_executor_terminal.go`, `codex_executor_stream.go`).
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
   - **Note**: This branch no longer injects the User-Agent stabilizer into `management.html`; the panel patch `patch/secure-storage-stable-key` owns that fix. Publish a panel release containing it before a core `main` without the injection, or browsers that already ran the injected migration lose their saved login.

8. **`patch/downstream-panel-source`**
   - **Goal**: Serve this fork's management panel by default.
   - **Scope**: `DefaultPanelGitHubRepository`, the default release lookup in `internal/managementasset/updater.go`, and `config.example.yaml` point at `JohnsonRan/Cli-Proxy-API-Management-Center`. An explicit `remote-management.panel-github-repository` still wins. The last-resort fallback page (`cpamc.router-for.me`) stays upstream's.

9. **`patch/fix-interrupt-test-race`**
   - **Goal**: Deflake `TestResponsesInterruptStopsHTTPUpstream`, which sampled a cancellation flag once and failed about one run in thirty.
   - **Scope**: Test-only (`sdk/api/handlers/openai/openai_responses_interrupt_test.go`). Retire when upstream fixes the test.

10. **`patch/claude-thinking-off`**
    - **Goal**: Turn Claude thinking off the way each model accepts.
    - **Scope**: `claude-sonnet-5-5` uses `thinking.type: "between_tools"`; `claude-opus-5-5`, `claude-fable-5`, `claude-fable-5-1` cannot turn thinking off and fall back to their lowest adaptive level with `display: "omitted"`; dated and bracketed Claude variants resolve to their catalog entry. The facts live in `internal/registry/claude_thinking_facts.go` and are overlaid on every loaded catalog, because the release workflow and the runtime updater replace `models.json` with the remote catalog.
    - **Related Upstream Work**: PR [#6571](https://github.com/router-for-me/CLIProxyAPI/pull/6571), PR [#6570](https://github.com/router-for-me/CLIProxyAPI/pull/6570).

11. **`patch/claude-search-recovery`**
    - **Goal**: Recover Claude conversations after OAuth credential failover when an earlier turn's encrypted `web_search_tool_result` is rejected with HTTP 400.
    - **Scope**: `internal/runtime/executor/helps/claude_search_recovery.go` and the Claude execute/stream paths, applied before user payload rules.
    - **Related Upstream Work**: PR [#6576](https://github.com/router-for-me/CLIProxyAPI/pull/6576).

12. **`patch/codex-oauth-resilience`**
    - **Goal**: Codex OAuth refresh resilience.
    - **Scope**: Cache completed refreshes for 5 minutes for late concurrent callers, stop retrying terminal `invalid_grant`/`refresh_token_invalidated`, send the Codex `User-Agent` on exchange/refresh (`internal/auth/codex/openai_auth.go`); classify `401 Your IP is not authorized` as a retryable `503 ip_temporarily_restricted` so it does not force a refresh.
    - **Related Upstream Work**: PR [#6447](https://github.com/router-for-me/CLIProxyAPI/pull/6447), PR [#6437](https://github.com/router-for-me/CLIProxyAPI/pull/6437), PR [#6165](https://github.com/router-for-me/CLIProxyAPI/pull/6165), PR [#5766](https://github.com/router-for-me/CLIProxyAPI/pull/5766), PR [#5850](https://github.com/router-for-me/CLIProxyAPI/pull/5850).

13. **`patch/embeddings-and-rerank`**
    - **Goal**: OpenAI-compatible `/v1/embeddings`, `/v1/embedding`, `/v1/rerank`, and `/rerank` for OpenAI-compatible and Gemini providers.
    - **Scope**: Routes, handlers, `OpenAICompatExecutor` embeddings/rerank execution, Gemini `:embedContent`/`:batchEmbedContents` translation, and rerank usage publication (OpenAI `usage` or `meta.tokens`/`meta.billed_units`). The model-level `embedding` and `rerank` flags are metadata for the panel's connectivity test; routing does not read them.
    - **Related Upstream Work**: Issue [#5961](https://github.com/router-for-me/CLIProxyAPI/issues/5961), Issue [#3641](https://github.com/router-for-me/CLIProxyAPI/issues/3641), Issue [#1084](https://github.com/router-for-me/CLIProxyAPI/issues/1084), PR [#1191](https://github.com/router-for-me/CLIProxyAPI/pull/1191).

14. **`patch/client-api-key-models`**
    - **Goal**: Visible and usable model access control globally and per client API key.
    - **Scope**:
      - `access.models` / `access.allowed-models` / `access.excluded-models` and per-key mapping entries in `access.api-keys`.
      - Filter `/v1/models` and model detail endpoints; reject execution, count-tokens and Codex Realtime with `403 model_not_allowed`.
      - Struct-based config saves fold per-key rules back onto `access.api-keys` (`foldClientKeysIntoAPIKeys`); comments attached to those mapping entries are not preserved.
      - `GET /v8/management/models` lists servable models without client-key filtering.
    - **Known limits**: Rules match the requested model name, before alias and prefix resolution. A key with its own allow-list does not inherit the global exclusion list.
    - **Cross-repo**: Required by the panel patch `patch/client-api-key-models`.
    - **Related Upstream Work**: PR [#6568](https://github.com/router-for-me/CLIProxyAPI/pull/6568), PR [#4030](https://github.com/router-for-me/CLIProxyAPI/pull/4030), PR [#5517](https://github.com/router-for-me/CLIProxyAPI/pull/5517).

## Retired Patch Branches

- `patch/p2-runtime-optimizations` was split into patches 3–7 on 2026-10-14.
- `patch/p0-oauth-and-executors` was split into patches 10–12 on 2026-10-14. Its multi-line Codex SSE assembly was dropped because upstream implemented `CodexSSEScanner` (Issue [#6575](https://github.com/router-for-me/CLIProxyAPI/issues/6575)).

## Rebuild & Validation Procedure

```bash
git fetch upstream main --tags
branches=(patch/http-compression patch/responses-ws-sse-incremental patch/chatgpt-utls-pool patch/runtime-memory-bounds patch/startup-model-registration patch/claude-api-key-policy patch/management-panel-update patch/downstream-panel-source patch/fix-interrupt-test-race patch/claude-thinking-off patch/claude-search-recovery patch/codex-oauth-resilience patch/embeddings-and-rerank patch/client-api-key-models)
git fetch origin ci "${branches[@]}"

git checkout -B main upstream/main
git merge --squash origin/ci || git restore --source=origin/ci --staged --worktree -- README.md
git commit -m "merge ci branch"
for branch in "${branches[@]}"; do
  git merge --squash "origin/${branch}" || { echo "conflict in ${branch}: fix it on the patch branch"; exit 1; }
  git diff --cached --quiet && { echo "${branch} is empty: check whether upstream absorbed it"; exit 1; }
  git commit -m "merge ${branch} branch"
done

# Must match the "Validate integrated build and patch tests" step in
# .github/workflows/upstream-sync.yml. The full ./internal/runtime/executor/...
# package is not run here: it has long-hanging tests, so it is covered by
# targeted -run patterns instead.
go build -o test-output ./cmd/server && rm -f test-output
go test ./sdk/api/handlers/... ./internal/api/middleware/... ./internal/api/handlers/management/... ./internal/config/... ./internal/watcher/diff/... ./internal/runtime/executor/helps/... ./internal/managementasset/... ./internal/thinking/... ./internal/registry/... ./internal/auth/codex/...
go test -run "TestServiceRun_RegistersLoadedAuthModelsBeforeListening" ./sdk/cliproxy
go test -run "TestCodexExecutorUpstreamRequestCompression|TestCodexAutoExecutorAllowsHTTPDownstreamToUseWebsocketUpstream|TestCollectCodexOutputItemDone|TestCodexOutputItemPatchBuffer|TestOfficialAPIKeyWithEmptyOrAutoCloakConfigDoesNotCloak|TestClassifyClaudeUpstreamErrorPromotesLowCreditBalance400To402|TestCodexTemporaryIPRestrictionClassifiedAs503|TestGeminiExecutor_Embeddings|TestOpenAICompatExecutor_Embeddings|TestOpenAICompatExecutor_Rerank" ./internal/runtime/executor
git push --force-with-lease origin HEAD:main
```

## CI/CD Workflows Owned by `ci`

- `.github/workflows/upstream-sync.yml`: Runs daily at `20:18 UTC` (`04:18 Asia/Shanghai`) and via `workflow_dispatch`. Rebuilds `main` from `upstream/main`, fails on a conflicting or empty patch, validates the candidate, and pushes with `--force-with-lease` only when the rebuilt tree differs from `main`.
- `.github/workflows/ghcr-docker.yml`: Builds and publishes multi-arch (`linux/amd64`, `linux/arm64`) images to `ghcr.io/johnsonran/cli-proxy-api` on `main` pushes, `v*` tags, `workflow_dispatch`, and `Upstream Sync` runs that actually moved `main`.

## Downstream Policy

- **No upstream pull requests.** This fork never opens pull requests against the upstream repository; the `Related Upstream Work` entries record upstream context only. Patches are maintained downstream indefinitely and are retired only when upstream lands an equivalent implementation, detected by a rebuild that finds the patch empty or conflicting.
- **`main` is intentionally unprotected.** The scheduled sync rebuilds and force-pushes `main`, so it carries no branch protection. Safety comes from `--force-with-lease`, the `upstream-sync-main` concurrency group, fail-closed conflict and empty-patch handling, and validation before the push. Protect the `patch/*` and `ci` branches against deletion instead; `main` is a derived artifact that nothing else may be based on.
- **Cross-repo patches.** Core and panel patches that depend on each other are listed under `Cross-repo` on both sides. Ship the provider side first.
