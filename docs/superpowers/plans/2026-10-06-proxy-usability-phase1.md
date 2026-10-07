# Account Proxy Usability — Phase 1 Plan

> **Review status:** Conditional approval received from `/data/projects/my-agent-workspace` on 2026-10-06; follow-up S1/S2 conditions are incorporated below. Reviewer said implementation may begin without another review round. This document plans Phase 1 (G1–G3) only; G4 probe-health scheduling is explicitly deferred to a separate Phase 2 plan.

**Goal:** Ensure an account with a configured proxy is never sent directly or selected for scheduling when that proxy is deleted, inactive, or expired.

**Architecture:** Keep the existing account-wide `IsSchedulable()` contract focused on account state. Reuse and extend the existing `resolveAccountProxyURL` function for request paths; make proxy lifecycle mutations refresh every affected account snapshot through scheduler outbox. Scheduler candidates use a minimal proxy metadata projection in Redis, never host/port/credentials. Preserve the custom-base-URL relay behavior while enforcing configured-proxy usability before any request is sent.

**Tech Stack:** Go, Ent/PostgreSQL repositories, Redis scheduler snapshots/outbox, existing unit-tagged tests (`go test -tags=unit -count=1`).

---

## 二轮修订（2026-10-06；逐项回应 R1–R6）

### R1 — 调度候选读 meta；meta 必须带最小代理状态

已更正 Q1：`schedulerCache.GetSnapshot` 读 `schedulerAccountMetaKey(id)`，不是完整账号 key；当前 `buildSchedulerMetadataAccount` 不复制 `ProxyID`/`Proxy`。因此 G3 只能在候选 meta 实际携带代理信息后实现，不能拿手工构造的 `Account` 测纯判断函数。

- Meta 新增 `ProxyID` 和最小 `Proxy`：只保留 `ID`、`Status`、`ExpiresAt`；禁止 host、端口、用户名、密码、完整 URL 或凭据进入 meta。到候选判定时用当前时间计算 `IsExpired(now)`，不缓存“已过期”布尔值。
- 判定固定为：`ProxyID == nil` → direct mode、可用；`ProxyID != nil && Proxy == nil` → 已删除；`Status != active` → 停用；`IsExpired(now)` → 过期。Repo/缓存写入者先保证代理关系已加载，故候选 meta 中 `Proxy == nil` 不再表示“未预加载”。
- 写入路径：`SetSnapshot` 与 `SetSnapshotAndReturnAccountIDs` 都经 `writeSnapshotVersionAndReturnAccountIDs` → `writeAccountIDs` → `marshalSchedulerCacheAccount` 写完整记录和 meta；`SetAccount` 直接经 `writeAccountIDs` 写两者。Rebuild 的 `loadAccountsForRebuild`/`loadAccountsFromDB` 取 DB accounts；list 路径经 `accountsToService`/`loadProxies`。Outbox 的 `handleBulkAccountEvent` 用 `GetByIDs`+`WithProxy` 后才 `SetAccount`。Repository 单条状态同步经 `GetByID`→`accountsToService`/`loadProxies`，批量同步经 `GetByIDs`+`WithProxy`。`UpdateAccountInCache` 接受调用方传入的 Account；在写 meta 前必须验证其来源已预加载，若 `ProxyID != nil && Proxy == nil` 则经 repo 重新加载，不能把部分对象直接写入。`SetSnapshotByAccountIDs` 只更新 bucket membership、不写 meta，不作为代理元数据写入路径。
- 旧版本过渡：旧 meta 缺少 `ProxyID`，启动全量 rebuild 前候选行为与当前一样，不额外变差；这段窗口由请求侧 G1 resolver 对选中账号再次 fail-closed 兜底。`SchedulerSnapshotService.Start` 的 `runInitialRebuild` → `loadAccountsForRebuild` → `setRebuildSnapshot` 为每个重建后仍引用的候选 ID 调 `SetSnapshotAndReturnAccountIDs`，从 DB 重写对应 meta。重建清除/替换旧 bucket membership 后，残留但不再被任何 bucket 引用的 meta 不影响选号。另在部署验收中验证初始 rebuild 完成后，所有活跃候选 bucket 的账号均读到新字段。
- Meta 回归测试必须从 `marshalSchedulerCacheAccount` + 实际 Redis 写入路径执行 `SetSnapshot`/`SetAccount`，再通过 `GetSnapshot` 读取候选并断言 `ProxyID`、`Proxy.ID/Status/ExpiresAt` 可见且凭据字段不存在；直接调用判断 helper 的手工 Account 测试不能代替。该测试需先在 `ca0946e91` 基线上实跑并记录原样红测输出，当前计划不声称红测已运行。

### R2 — 改造已有 resolver，不另起同名函数

改造 `backend/internal/service/gateway_websearch_emulation.go` 中已有 resolver，并在 service 包定义窄接口 `type proxyGetter interface { GetByID(ctx context.Context, id int64) (*Proxy, error) }`。目标签名为 `resolveAccountProxyURL(ctx context.Context, proxies proxyGetter, account *Account) (string, error)`。持有 `proxyRepo` 的 CN balance/quota、OpenAI quota、Grok quota、Gemini OAuth 调用方传入该 repo；没有 repo 依赖的 Gateway、OpenAI Gateway、AccountTest、AccountUsage、Antigravity 调用方传 `nil`，不新增 wire 依赖。传 nil 且非空 `ProxyID` 的 Account 缺少预加载 Proxy 时返回 lookup-failed 并拒绝。对 getter 错误用 `errors.Is(err, ErrProxyNotFound)` 识别 deleted，其余错误是 lookup-failed。4 个现有调用者 `OpenAIGatewayService.createUpstreamLiveCall`、`OpenAIGatewayService.dialLiveSideband`、`doWebSearch`、`OpenAIGatewayService.ResolvePluginOutboundIdentity` 仍纳入 R6 迁移表。不创建第二个同名 resolver。

### R3 — 软删除语义已定，不增加 load-state 字段

Ent `Proxy` 使用 `SoftDeleteMixin`；`accountRepository.loadProxies` 的查询与 `GetByIDs` 的 `WithProxy()` 都过滤软删除代理，因此已删关系表现为 nil。请求路径：对 `ProxyID != nil && Proxy == nil` 使用传入的 `proxyGetter`；nil getter 归类为 lookup-failed，`errors.Is(err, ErrProxyNotFound)` 归类为 deleted，其他查询错误归类为 lookup-failed，所有情况都拒绝且绝不降级直连。调度路径仅在 R1 所列写入来源完成预加载保证后，才把候选 meta 的 nil Proxy 解释为 deleted。保留测试钉住这项 Ent 语义，不再把加载标志位与删除标记作为待选项。

### R4 — P1–P5 的一期决定

| 判据 | 一期决定 | 必须保持的行为 |
|---|---|---|
| P1 | fail-closed | 配置了 ProxyID 却无法解析/使用代理时，不构造直连 transport，也不发送请求。 |
| P2 | 复用 `*UpstreamFailoverError` | resolver 返回该类型，并使用专用的 proxy-unavailable `Reason`/`Stage`；`RetryableOnSameAccount=false`，使 `ShouldRetryNextAccount()` 触发换号、不触发同账号冷却。包装只能用 `%w`。gateway handlers（如 `GatewayHandler.Messages`、`ChatCompletions`、`Responses` 和 `OpenAIGatewayHandler.Responses`）现有 `errors.As(err, &failoverErr)` 分支识别它，再交给 `FailoverState.HandleFailoverError`；须在每个迁移的返回路径确认该错误直接到 handler，不送入 `classifyUpstreamTransportError`。不调用 `TempUnschedulableUntil` 写入路径，不调用限流服务；只记录 proxy ID、原因和 account ID，不记录 URL/凭据。 |
| P3 | 接受 `FallbackModeNone`/未解 fallback 下的账号不调度与请求拒绝 | 保持绑定行，不再继续使用坏代理；Task 5 更新后台说明。 |
| P4 | 不加功能开关 | 部署前按 Q7 只读统计，让用户决定是否预修数据。 |
| P5 | 二期另议 | G4 探测阈值、恢复规则、探测服务整体故障保护及池代理参与度均不在一期实现。 |

非网关调用方不转换成换号错误：账号测试返回既有结构化测试失败结果；余额/用量/配额请求将错误返回其管理 API/任务结果且不写 usage/credit 成功值；token refresh 返回既有刷新失败结果并由刷新任务按原失败通道记录。以上都在调用边界写一条脱敏 warning（account ID、proxy ID、deleted/inactive/expired/lookup-failed），不把此错误传给 transport classifier、不置账号 TempUnsched、不更新 rate-limit/error 状态、不直连重试。接受 N3：OpenAI handler 现有 `ShouldReportAccountScheduleFailure` 可能将 proxy-unavailable 纳入账号调度失败统计；这是可观察的调度指标，不视为 rate-limit/error/TempUnsched 状态，不为此修改 `ShouldReportAccountScheduleFailure`。若某个非网关调用方不能以此策略安全表达失败，实施时须在该 caller 的公开返回类型内显式映射，不能吞错。

按 N4 共享纯判定函数 `proxyUnavailableReason(proxyID *int64, p *Proxy, now time.Time) string`：请求 resolver 在完成必要 getter 回查后与调度 meta gate 均调用它，唯一映射 direct/deleted/inactive/expired 规则；getter 查询失败由 resolver 转为 lookup-failed，不由纯函数推断。

### R5 — 宽 grep 与唯一白名单

验收基线使用：`grep -rnE '\.Proxy != nil' backend/internal/service --include='*.go' | grep -v _test.go`。实施后允许恰好 **1** 行：集中 resolver `gateway_websearch_emulation.go::resolveAccountProxyURL` 内对已解析关系的检查，理由是该函数是唯一允许访问 `Account.Proxy` 关系的封装边界。其余任何变量名/表达式（包括 `latest.Proxy != nil`、`case account.Proxy != nil`、`if account.Proxy != nil` 和作为 bool 参数的判断）均迁移；最终 grep 行数必须等于白名单 1，不能用更窄模式或仅 `account.Proxy != nil` 验收。

### R6 — 逐处迁移表、批次与定向测试

下表覆盖基线 grep 的 75 个非测试命中，并同时列出少数只调用集中 helper、自己不含 grep 命中的间接调用者（明确标为传播调用者；测试文件不计入 75）。普通转发方法的既有 `error` 返回逐层 `%w` 到 gateway handler；非网关批次依 P2 返回本域的结构化失败；只返回 string 的 helper 改为 `(string, error)`；WS bool 实参改成在调用层先解析并传 URL/错误，不再把 eligibility 压成 bool。表中同一批次的多个函数共享一个定向验证命令，新增测试名在对应任务先创建，再执行命令。

| 批次 / 文件 | 函数与当前 `.Proxy` 用途 | 错误传播与定向验证 |
|---|---|---|
| A1 `account_test_service.go` | `testClaudeAccountConnection`、`testClaudeVertexServiceAccountConnection`、`testBedrockAccountConnection`、`testOpenAIAccountConnection`、`testOpenAIChatCompletionsConnection`、`testOpenAICompactConnection`、`testGeminiAccountConnection`、`testOpenAIImageAPIKey`、`testOpenAIImageOAuth`：构建测试请求的代理 transport；`grokTestProxyURL`：只返回 URL 的 string helper。 | 前九个向测试结果返回错误；helper 改为 `(string,error)` 并由调用者传播。只返回结构化测试失败、不写账号状态。定向：`go test -tags=unit -count=1 ./internal/service -run '^(TestProxyUnavailableAccountTest|TestGrokTestProxyURLUnavailable)$'`。|
| A2 `account_test_service_cn_adaptive.go`、`account_test_service_typesafe.go`、`antigravity_gateway_service.go`、`openai_apikey_responses_probe.go` | `doCNProviderAdaptiveRequest`、`testTypeSafeAccountConnection`、`AntigravityGatewayService.TestConnection`、`ProbeOpenAIAPIKeyResponsesSupport`：账号测试/能力探测 outbound proxy URL。 | 错误返回各自测试/探测结果，不改账号调度状态。定向：`go test -tags=unit -count=1 ./internal/service -run '^(TestProxyUnavailableCNAdaptiveTest|TestProxyUnavailableTypeSafeTest|TestProxyUnavailableAntigravityTest|TestProxyUnavailableResponsesProbe)$'`。|
| A3 `account_usage_service.go`、`cn_provider_balance_service.go`、`cn_provider_quota_service.go`、`grok_quota_service.go`、`openai_quota_service.go` | `probeOpenAICodexSnapshot`、`fetchOAuthUsageRaw`、`CNProviderBalanceService.resolveProxyURL`、`CNProviderQuotaService.resolveProxyURL`、`GrokQuotaService.resolveProxyURL`、`OpenAIQuotaService.prepareUpstreamCall`：usage/balance/quota API 的代理 URL；两个 CN `resolveProxyURL` 按现有签名迁移。 | helper 错误返回调用者；usage/credit 只在远端成功后写入，不因 proxy 错误写成功值或限流状态。定向：`go test -tags=unit -count=1 ./internal/service -run '^(TestProxyUnavailableUsage|TestProxyUnavailableQuota|TestProxyUnavailableCNBalance)$'`。|
| A4 `gemini_oauth_service.go`、`openai_agent_identity.go`、`grok_credential_failure.go`、`openai_codex_models_service.go` | `RefreshAccountGoogleOneTier`：token refresh；`registerAgentIdentityTask`：后台身份注册 outbound；`validateCurrentGrokCredentialFailure`：以 `latest.Proxy` 关系判断异步失败是否仍适用；`FetchCodexModelsManifest`：模型清单请求。 | refresh/后台任务走既有失败返回和脱敏日志，不设 TempUnsched/限流；Grok stale-event 检查改为显式有效性检查/共享解析，不保留 nil 检查。定向：`go test -tags=unit -count=1 ./internal/service -run '^(TestProxyUnavailableTokenRefresh|TestProxyUnavailableAgentIdentity|TestGrokCredentialFailureProxyState|TestProxyUnavailableCodexModels)$'`。|
| B1 `antigravity_gateway_claude.go`、`antigravity_gateway_gemini.go`、`antigravity_gateway_upstream.go` | `AntigravityGatewayService.Forward`、`ForwardGemini`、`ForwardUpstream`：gateway upstream transport。 | `%w` 返回 proxy `UpstreamFailoverError` 到对应 handler；不进入 transport classifier。定向：`go test -tags=unit -count=1 ./internal/service -run '^TestProxyUnavailableAntigravityGateway$'`。|
| B2 `gateway_anthropic_passthrough.go`、`gateway_bedrock.go`、`gateway_count_tokens.go`、`gateway_forward.go`、`gateway_forward_as_chat_completions.go`、`gateway_forward_as_responses.go`、`gateway_service.go`、`gateway_systemone.go`、`gateway_upstream_request.go` | `forwardAnthropicAPIKeyPassthroughWithInput`、`forwardBedrock`、`ForwardCountTokens`、`forwardCountTokensAnthropicAPIKeyPassthrough`、`GatewayService.Forward`、`ForwardAsChatCompletions`、`ForwardAsResponses`、`DoGrokNativeResponsesJSON`、`ForwardSystemOne`、`buildCustomRelayURL`：各协议正常请求、count-token、自定义 relay 的 proxy transport/relay 参数。 | 解析错误须在构造 request/relay 和写 response body 前返回到 gateway handler；custom URL 仍带有效 proxy 参数，坏 proxy 不构造无代理 relay。定向：`go test -tags=unit -count=1 ./internal/service -run '^(TestProxyUnavailableGateway|TestProxyUnavailableCustomBaseURL)$'`。|
| B3 `gemini_chat_completions_compat_service.go`、`gemini_messages_compat_service.go`、`openai_alpha_search.go` | `forwardClaudeBodyAsChatCompletions`、`GeminiMessagesCompatService.Forward`、`ForwardNative`、`ForwardAIStudioGET`、`ForwardAlphaSearch`：compat、native、search upstream transport；`RefreshAccountGoogleOneTier` 是 A4 中的唯一命中，不在本批重复计算。 | 网关方法包装传递至 Gemini/OpenAI 对应 handler；refresh 只返回 refresh failure。定向：`go test -tags=unit -count=1 ./internal/service -run '^(TestProxyUnavailableGeminiCompat|TestProxyUnavailableAlphaSearch)$'`，refresh 用 A4 命令。|
| B4 `grok_audio.go`、`grok_media.go`、`openai_embeddings.go`、`openai_gateway_cc_pipeline.go`、`openai_gateway_chat_completions.go`、`openai_gateway_chat_completions_anthropic_native.go` | `ForwardGrokVoice`、`OpenGrokRealtime`、`ProbeGrokRealtime`、`ForwardGrokMedia`、`forwardGrokMediaVideoContent`、`ForwardEmbeddings`、`sendCCUpstreamRequest`、`forwardAsChatCompletions`、`forwardChatCompletionsViaNativeAnthropic`：音频/媒体/embedding/CC/chat 上游代理。 | 普通 gateway 返回 failover error；Grok realtime/媒体在已写出或已升级连接前检查，错误映射到其现有 failover/WS 关闭协议。定向：`go test -tags=unit -count=1 ./internal/service -run '^(TestProxyUnavailableGrokAudio|TestProxyUnavailableGrokMedia|TestProxyUnavailableOpenAIEmbeddings|TestProxyUnavailableOpenAIChat)$'`。|
| B5 `openai_gateway_count_tokens.go`、`openai_gateway_forward.go`、`openai_gateway_grok.go`、`openai_gateway_grok_chat_bridge.go`、`openai_gateway_messages.go`、`openai_gateway_messages_anthropic_native.go` | `ForwardResponsesInputTokens`、`ForwardCountTokensAsAnthropic`、`OpenAIGatewayService.Forward`、`forwardGrokResponses`、`describeGrokComposerImage`、`forwardGrokChatCompletionsViaResponses`、`ForwardAsAnthropic`、`forwardAnthropicViaNativeAnthropicEndpoint`：OpenAI/Grok/Anthropic routes 的 outbound URL。 | 向上返回可由各 handler 的 `errors.As` 捕获的 `*UpstreamFailoverError`。定向：`go test -tags=unit -count=1 ./internal/service -run '^(TestProxyUnavailableOpenAIGateway|TestProxyUnavailableGrokBridge|TestProxyUnavailableAnthropicGateway)$'`。|
| B6 `openai_gateway_passthrough.go`、`openai_gateway_responses_anthropic_native.go`、`openai_images.go`、`openai_images_b64_backfill.go`、`openai_images_responses.go`、`seedance.go` | `forwardOpenAIPassthrough`、`forwardResponsesViaNativeAnthropic`、`forwardOpenAIImagesAPIKey`、`fetchOpenAIImageURLBase64`、`forwardOpenAIImagesOAuth`、`ForwardSeedance`：passthrough、image fetch/generate、Seedance 上游代理。 | 在外部 HTTP 请求前返回 failover error；响应/图片结果不伪装为成功。定向：`go test -tags=unit -count=1 ./internal/service -run '^(TestProxyUnavailableOpenAIPassthrough|TestProxyUnavailableOpenAIImages|TestProxyUnavailableSeedance)$'`。|
| B7 `gateway_websearch_emulation.go`、`openai_live.go`、`openai_plugin_account_directory.go`、`upstream_models.go` | grep 命中：`resolveAccountProxyURL`（helper 实现）和 `upstreamModelsProxyURL`（string-only helper）。传播调用者（不另计入 75）：`doWebSearch`、`OpenAIGatewayService.createUpstreamLiveCall`、`dialLiveSideband`、`ResolvePluginOutboundIdentity`、`fetchAntigravityOAuthUpstreamModels`、`fetchModelsDevRegistry`、`fetchUpstreamModelList`、`FetchOpenAIModelsList`。 | 改造已有 `resolveAccountProxyURL(ctx,proxies,account)(string,error)` 的 4 个调用者；`upstreamModelsProxyURL` 改为 `(string,error)`，四个调用者传播或返回后台模型同步错误。定向：`go test -tags=unit -count=1 ./internal/service -run '^(TestResolveAccountProxyURL|TestProxyUnavailableLiveCall|TestProxyUnavailableSideband|TestProxyUnavailablePluginIdentity|TestProxyUnavailableModelDiscovery)$'`。|
| C1 WS bool-argument sites `openai_ws_forwarder_ingress.go`、`openai_ws_forwarder_v2.go`、`openai_ws_v2_passthrough_adapter.go` | `ProxyResponsesWebSocketFromClient`（含 :799 URL 与 :906 bool）；`forwardOpenAIWSV2`（含 :199 bool、:215 URL、:251 bool）；`proxyResponsesWebSocketV2Passthrough`（含 :827 bool、:860 URL）。共 7 处 grep 命中；其中 4 处 bool 实参先前把 Proxy configured 压为布尔值。 | 在 WS ingress/forward 方法中解析一次并把 URL 与 error 明确向前传；删掉 bool 参数/实参，proxy error 在 upgrade/dial/write 前走 WS failover 错误路径，不把 bool=false 当 direct。定向：`go test -tags=unit -count=1 ./internal/service -run '^(TestProxyUnavailableOpenAIWebSocket|TestProxyUnavailableWebSocketV2|TestProxyUnavailableWebSocketPassthrough)$'`。|

Review note: 上表按 grep 中的每个命中列函数；同一函数有多处判断的，函数名只列一次但批次测试覆盖每处。基线中的 `token_refresh_service_test.go` 命中不计入 75 行非测试统计。任何函数签名或返回策略若与当前源码不符，实施前先在该函数做影响核对并同步修订本表，不能静默跳过。

## Baseline evidence and answers required by the criteria

All code-graph claims below come from plane `sub2api`, synchronized at `ca0946e91755`; graph impact counts are lower bounds because unresolved/dynamic calls are omitted.

### Q1 — What the scheduler snapshot contains

- `schedulerCache.GetSnapshot` reads `schedulerAccountMetaKey(id)` for each candidate, not the full account key. Current `buildSchedulerMetadataAccount` omits both `ProxyID` and `Proxy`; a candidate-only eligibility check against the present meta would therefore mistake bound accounts for direct mode. The full account record read by `GetAccount` is not the record used to build the candidate list.
- `SetSnapshot` and `SetSnapshotAndReturnAccountIDs` flow through `writeSnapshotVersionAndReturnAccountIDs` → `writeAccountIDs` → `marshalSchedulerCacheAccount`, which writes full account and meta keys. `SetAccount` also flows through `writeAccountIDs`. `SetSnapshotByAccountIDs` changes bucket membership only and does not write account/meta payloads.
- Rebuild candidates are loaded through `SchedulerSnapshotService.loadAccountsForRebuild`/`loadAccountsFromDB` and repository list paths that map via `accountsToService`/`loadProxies`. `handleBulkAccountEvent` loads IDs with `GetByIDs`+`WithProxy` before `SetAccount`. Repository single-account cache sync uses `GetByID`→`accountsToService`/`loadProxies`; bulk sync uses `GetByIDs`+`WithProxy`. `UpdateAccountInCache` is an additional writer accepting a caller-provided Account; before writing it must ensure a non-nil `ProxyID` has a loaded relation, reloading from the repository if necessary.
- The meta projection must carry `ProxyID` plus a `Proxy` containing only `ID`, `Status`, and `ExpiresAt`. No host, port, username, password, URL, or credentials. Runtime candidate evaluation is: `ProxyID == nil` → direct mode/available; non-nil `ProxyID` with nil `Proxy` → deleted; `Status != active` → inactive; `IsExpired(now)` → expired. Calculate expiry at read/evaluation time; do not persist a time-relative `expired` boolean.
- Ent `SoftDeleteMixin` filters soft-deleted proxies from `loadProxies` and `WithProxy`; that behavior is confirmed in Q4. The nil-as-deleted scheduler rule is safe only after each meta writer above guarantees proxy preloading. For request-side partial Accounts, use the R3 lookup path instead.
- Legacy meta has no `ProxyID`; until initial rebuild, its candidate treatment remains as today and request-side G1 resolution is the fail-closed guard. `Start` → `runInitialRebuild` → `loadAccountsForRebuild` → `setRebuildSnapshot` rewrites meta for every ID in the rebuilt active buckets through `SetSnapshotAndReturnAccountIDs`; old meta keys no longer referenced by any bucket cannot enter candidate selection. Acceptance checks all active bucket IDs after the rebuild rather than treating orphan metadata as candidates.

### Q2 — Which proxy mutations invalidate scheduler snapshots

- `github.com/Wei-Shaw/sub2api/internal/service.adminServiceImpl.UpdateProxy` merges new `Status`/`ExpiresAt` values and calls `proxyRepo.Update`.
- `github.com/Wei-Shaw/sub2api/internal/repository.proxyRepository.Update` reaches `updateProxyAndInvalidateProbeSnapshots`. When probe identity changes, that helper updates account `extra` fields only for API-key rows carrying specific billing/usage probe snapshots; the returned IDs are passed to `enqueueProxyProbeAccountChanges`. This is not an invalidation of every live account attached to the proxy. A name/expiry-only edit may not even change `proxyProbeIdentity`; an active/inactive change does. Therefore regular scheduler account snapshots are not reliably refreshed for every affected account on proxy status/expiry changes.
- Admin `DeleteProxy` checks the number of bound accounts before deleting; repository `Delete` itself soft-deletes by ID and does not enqueue account changes. Pool-proxy delete-if-unused is separately guarded and is expected to have no live account references.
- Phase 1 must enumerate all mutation paths (admin update, admin delete, restore/undelete if present, expiry sweep and direct repository deletion), identify affected live account IDs, and enqueue account-bulk-changed (or a bounded equivalent) transactionally with the mutation. Expiry sweep already performs account reassignments and emits bulk account changes for changed accounts; accounts retained on `FallbackModeNone` or unresolved fallback remain a critical scenario to keep in the affected set.

### Q3 — `IsSchedulable()` impact and why not to add proxy logic there blindly

`impact` on `github.com/Wei-Shaw/sub2api/internal/service.Account.IsSchedulable` at depth 6 returns 9 direct callers, 21 at distance 2, 17 at distance 3, 12 at distance 4, 19 at distance 5, and 4 at distance 6; the depth-6 group is still non-empty, so this is only a lower bound. Direct/nearby callers include gateway schedulers, `OAuthRefreshAPI.RefreshIfNeeded`, `CNProviderBalanceCheckService.checkOne`, token-refresh eligibility, sticky-session cleanup, rate-limit mutation, and plugin account information. The check is also used in status/reporting code, not only user-request scheduling. Do not add repository I/O or proxy eligibility directly to `IsSchedulable()`.

The implementation review must classify each of those behavior categories:

- User request schedulers, sticky/previous-response account selection, model selection, media selection, and scheduler snapshot candidate filtering: exclude unavailable-proxy accounts.
- OAuth refresh, token refresh, quota/balance/usage probes: enforce fail-closed transport if they make an upstream request with the configured account proxy, but do not necessarily stop the background job from being selected unless its own selection contract explicitly requires the proxy-aware scheduling gate. Record this per caller in the implementation review.
- Admin/status/plugin serialization and rate-limit-state updates: do not make proxy failure mutate account schedulability or rate-limit state; surface only an appropriate availability reason where the UI/API already reports eligibility.

### Q4 — Loaded proxy versus genuinely deleted/missing proxy

- `GetByID` goes through `accountsToService`/`loadProxies`; `GetByIDs` explicitly uses `WithProxy()`; `ListSchedulable` and related DB list paths also go through `accountsToService`. In all these repository-loaded paths a soft-deleted proxy is omitted, so the relation is nil.
- Partial/manual Accounts remain possible outside these loaders. Request-time resolution therefore treats `ProxyID == nil` as direct mode, but for `ProxyID != nil && Proxy == nil` uses the passed `proxyGetter`: nil getter is lookup-failed; `errors.Is(err, ErrProxyNotFound)` means deleted; any other lookup error means lookup-failed. Both fail closed and return the proxy-unavailable failover error; neither may become an empty URL/direct request.
- Scheduler meta does not need a separate “relation loaded” bit: R1 fixes all candidate meta writers to use the repository loaders or reload an incomplete passed Account before writing. Under that invariant only, candidate meta `ProxyID != nil && Proxy == nil` means the soft-deleted relation is missing. Keep an Ent soft-delete test to pin this invariant.

### Q5 — Shadow accounts

Shadows carry their own synchronized `ProxyID` and `Proxy` copied from the parent: creation copies `parent.ProxyID`, and `propagateProxyToShadows` updates shadow rows when the parent proxy changes; direct proxy edits to a shadow are rejected. Request resolver and scheduler candidate gate therefore use each shadow row’s own proxy fields, with no `resolveCredentialAccount` or `lookupShadowParentAccount` lookup for proxy eligibility. Tests cover invalid proxy states on the shadow’s own row and assert that parent proxy changes leave parent/shadow ProxyIDs equal.

### Q6 — Custom base URL

`GatewayService.Forward` currently bypasses `proxyURL` setup when custom base URL mode is active because `buildCustomRelayURL` carries the proxy as a relay query parameter. Preserve that transport contract, but run the same configured-proxy validity check before building/sending the custom relay request. Ensure no invalid/missing configured proxy falls through to the no-proxy custom relay URL.

### Q7 — Pre-deploy SQL (count only)

The production deployment record supplied on 2026-10-06 reports this count as zero for soft-deleted proxies. Before Phase 1 deployment, run this count-only query against the target database and report only the integer; do not select or print account/proxy fields:

```sql
SELECT COUNT(*)
FROM accounts a
JOIN proxies p ON p.id = a.proxy_id
WHERE a.deleted_at IS NULL
  AND p.deleted_at IS NULL
  AND (p.status <> 'active' OR (p.expires_at IS NOT NULL AND p.expires_at <= NOW()));
```

The `p.deleted_at IS NULL` predicate intentionally excludes deleted proxies from this Q7 count: the deployment handoff asks for live accounts tied to inactive/expired but still-present proxies. Deleted-proxy references remain covered by the original separate soft-delete count if the deployment checklist asks for it.

---

## Proposed implementation sequence (Phase 1 only)

### Task 1 — Pin down proxy relation load/deletion semantics and failover contract

**Files:**
- `backend/internal/service/account.go`, `proxy.go`, `proxy_service.go`, `gateway_service.go`
- `backend/internal/repository/account_repo.go`, `proxy_repo.go`
- `backend/ent/schema/proxy.go` and the existing soft-delete query/interceptor tests

- [ ] Add/retain focused tests proving `GetByID`/`accountsToService`+`loadProxies`, `GetByIDs`+`WithProxy`, and `ListSchedulable` omit soft-deleted proxies. Confirm a manually built partial Account can still have non-nil `ProxyID` and nil `Proxy`; request resolution must re-fetch that case.
- [ ] Add and unit-test the pure `proxyUnavailableReason(proxyID *int64, p *Proxy, now time.Time) string` helper; request resolution and scheduler meta eligibility must call this same function so state rules cannot drift.
- [ ] Reuse `*UpstreamFailoverError`; add a dedicated proxy-unavailable reason/stage, safe client message and log-only reason. Set `RetryableOnSameAccount=false` and the next-account action so `ShouldRetryNextAccount()` is true. Wrapped propagation must use `%w`, allowing existing gateway `errors.As(err, &failoverErr)` branches to recognize it. Do not introduce a separate proxy error that handlers will not recognize.
- [ ] Verify `FailoverState.HandleFailoverError` will not call `TempUnscheduleRetryableError` when `RetryableOnSameAccount` is false. Proxy errors are not transport failures: they return before outbound I/O and bypass `classifyUpstreamTransportError`; they never mutate temporary-unschedulable or rate-limit state.
- [ ] Keep only proxy ID/reason/account ID in logs and the error metadata; never include URL, host, port, credentials, authorization data, or full proxy entities.

### Task 2 — Reuse the request resolver and fail closed at every outbound call

**Files:**
- Extend `backend/internal/service/gateway_websearch_emulation.go::resolveAccountProxyURL` to `resolveAccountProxyURL(ctx context.Context, proxies proxyGetter, account *Account) (string, error)`; define the narrow `proxyGetter` interface in the service package and do not create another function with this name.
- Migrate every non-test grep hit listed in the R6 table, including account tests, usage/quota/balance, token refresh, gateway protocols, websocket bool arguments, custom relay, and model discovery.
- `backend/internal/service/upstream_models.go::upstreamModelsProxyURL` and `AccountTestService.grokTestProxyURL` are string-only helpers today; change both to `(string, error)` and update their callers. Keep errors unwrapped only at explicitly mapped non-gateway API/task boundaries; use `%w` for gateway propagation.

- [ ] Write helper tests for direct mode, active proxy, deleted/not-found, lookup error, inactive and expired proxy. At each representative gateway/account-test outbound boundary assert the fake transport receives zero calls for every invalid configured proxy.
- [ ] Before implementation, run these new tests against untouched `ca0946e91` and save exact failing output in the eventual implementation review; this plan does not claim the red run has happened.
- [ ] Resolver uses the Account passed by its caller without resolving a shadow parent: shadows carry their own synchronized `ProxyID` and `Proxy`, as established by S2. No `ProxyID` → empty URL/direct allowed; preloaded valid Proxy → URL; non-nil `ProxyID` with nil relation → call the passed `proxyGetter`; nil getter or `errors.Is(err, ErrProxyNotFound)` maps respectively to lookup-failed or deleted; any other getter error → lookup-failed; inactive or expired → matching failover error. Never translate any error into empty URL.
- [ ] For gateway calls, return/wrap `*UpstreamFailoverError` before transport construction, request write, or websocket upgrade/dial. Verify each actual return reaches the handler’s `errors.As` failover branch, not `classifyUpstreamTransportError`. For custom base URL mode, validate the configured proxy first and only then build the existing relay URL with the valid proxy parameter.
- [ ] For non-gateway calls follow the R4 policy and R6 caller table: return structured account-test/usage/quota/refresh failure, emit sanitized warning, and do not change rate-limit or account schedulability state.
- [ ] Migrate in the R6 batches; after each batch run its listed targeted tests. Final static acceptance uses the exact R5 grep and one-line whitelist.

### Task 3 — Make scheduling proxy-aware without widening `IsSchedulable()`

**Files:**
- `backend/internal/repository/scheduler_cache.go` and `scheduler_cache_unit_test.go`
- `backend/internal/service/scheduler_snapshot_service.go`, `gateway_scheduling.go`, `openai_account_scheduler.go`, `openai_gateway_scheduling.go`, `gemini_messages_compat_service.go`
- Related repository and scheduler snapshot tests

- [ ] First add a cache regression that writes a proxy-bound Account through actual `marshalSchedulerCacheAccount` → `SetSnapshot`/`SetAccount` → Redis → `GetSnapshot`; assert candidate meta carries `ProxyID` and only `Proxy.ID`, `Status`, `ExpiresAt`. Run the test against base `ca0946e91` before implementation and record the exact red output. Do not substitute a direct call to the eligibility predicate with a hand-built Account.
- [ ] In `buildSchedulerMetadataAccount`, copy `ProxyID` and create a metadata-only `Proxy` carrying `ID`, `Status`, `ExpiresAt`; do not serialize URL/host/port/user/password/credentials into meta. Recheck through `GetSnapshot`, since that is the actual candidate read path.
- [ ] Add a pure candidate eligibility check using meta values and current time. Gate scheduler candidate lists for each account’s own deleted, non-active and expired proxy; no repository lookup per candidate. Preserve `Account.IsSchedulable()` as account-only behavior.
- [ ] In every writer, preserve R1’s loader invariant. For `UpdateAccountInCache`, if `ProxyID != nil && Proxy == nil`, reload through repository before `SetAccount`; fail the cache update rather than writing ambiguous metadata if reloading fails. Exercise rebuild, outbox bulk update, single/bulk repository sync, and partial-account update paths.
- [ ] At startup, confirm `runInitialRebuild` completes a full bucket rebuild and rewrites all active bucket-referenced meta through `SetSnapshotAndReturnAccountIDs`. Test that no stale membership references legacy metadata afterward; unreferenced old meta keys are harmless and not candidates. Until this rebuild is complete, request-side resolver is the G1 backstop described in R1.
- [ ] Treat a shadow candidate exactly like a non-shadow candidate for proxy eligibility: its own Redis meta has its synchronized `ProxyID` and minimal `Proxy` projection. Do not use `lookupShadowParentAccount` to resolve proxy eligibility; that helper remains for the existing parent-health/credential logic only. Add a propagation test proving `propagateProxyToShadows` leaves the parent and all shadows with the same updated ProxyID, then validate each account’s own meta.
- [ ] Test no-proxy and active proxies remain selectable, and deleted/inactive/expired proxies on a shadow’s own row exclude that shadow. Classify `IsSchedulable()` callers without changing background refresh/quota selection.

### S1 — Getter dependency is explicit; no service wiring expansion

`resolveAccountProxyURL` receives a `proxyGetter` interface as its second argument. Proxy-aware service callers pass their existing repository dependency; gateway/test/usage services without one pass nil and fail closed if a bound Account reaches the resolver without a preloaded Proxy. Not-found is detected with `errors.Is(err, ErrProxyNotFound)`; every other lookup failure is fail-closed. This resolves R3 lookup semantics without adding fields to service constructors or DI wiring.

### S2 — Shadows use their own propagated proxy fields

Shadow creation copies the parent `ProxyID`, and `propagateProxyToShadows` updates it with the parent. Request resolution and scheduler meta checks use the shadow row’s own synchronized `ProxyID`/`Proxy`; they do not fetch or validate the parent for proxy eligibility. `proxy_id = changedProxyID` mutation queries already include parent and shadow rows. Acceptance includes a regression proving propagation keeps parent/shadow `ProxyID` values equal after a proxy change.

### Task 4 — Invalidate/rebuild snapshots on proxy lifecycle changes

**Files:**
- `backend/internal/repository/proxy_repo.go`
- `backend/internal/service/admin_proxy.go`
- `backend/internal/service/scheduler_snapshot_service.go`
- Relevant proxy repository/admin/outbox tests.

- [ ] Add failing tests showing proxy `Status`/`ExpiresAt` update leaves attached account snapshot stale on the current base, and proxy deletion does not leave an eligible stale cache entry.
- [ ] From the mutation transaction, enumerate all live accounts with `proxy_id = changedProxyID`; this set already includes shadows because their own ProxyID is synchronized with the parent. Enqueue bounded `account_bulk_changed` events for every matching account atomically with the committed mutation. Separately test parent proxy propagation (`propagateProxyToShadows`) so updates continue to keep the shadow rows in this query result.
- [ ] Ensure status change, expiry edit/clear, deletion, restore, and expiry sweep with accounts retained due to `FallbackModeNone` or unresolved fallback all refresh snapshots. Preserve expiry sweep’s existing account reassignment semantics and avoid duplicate event storms.
- [ ] Verify default outbox polling latency (1 second if config unset) and configured production value; report the measured max refresh delay and trigger in the delivery note. A periodic full rebuild is recovery, not the primary guarantee.

### Task 5 — Document the changed expiry semantics and operator action

**Files:**
- The existing proxy fallback/admin documentation discovered during implementation (likely `docs/` and/or `deploy/` proxy documentation).
- Admin UI helper text only if the existing UI currently describes `FallbackModeNone` as continuing to use the expired proxy; identify exact component after repository search.

- [ ] Document that with `FallbackModeNone`, an expired proxy remains bound but its account is now non-schedulable and outbound calls fail over rather than going direct.
- [ ] Document the same behavior for unresolved fallback chains and inactive/deleted proxies.
- [ ] Keep the change behind no feature flag; before release, run and report count-only Q7 SQL so the operator can decide whether to repair bindings before rollout.

### Task 6 — Focused tests and Phase 1 acceptance

**Tests likely to add/extend:**
- New helper tests adjacent to `account`/proxy resolver tests in `backend/internal/service`.
- New direct-transport fail-closed tests in representative gateway and account test/usage service suites.
- Scheduler cache/outbox tests in `backend/internal/service/scheduler_snapshot_service*` and repository proxy mutation tests in `backend/internal/repository`.
- Shadow behavior in existing OpenAI/Spark shadow test suites.

**Existing tests expected to remain unchanged unless a test explicitly asserts the old unsafe behavior:**
- `backend/internal/service/account_quota_schedulable_test.go` and `backend/internal/service/temp_unsched_test.go`: generic `Account.IsSchedulable()` semantics remain unchanged; proxy availability is a separate decision.
- Existing request tests with `ProxyID == nil` and valid proxies must remain unchanged.
- Tests for `FallbackModeNone`/unresolved expiry that currently assert the binding remains in storage should keep that repository assertion; add separate assertions that the account cannot be selected or sent directly.
- Any existing test explicitly expecting a non-nil `ProxyID` plus missing/inactive/expired `Proxy` to generate a direct request must be changed only after documenting the exact test and old behavior; locate these by searching test files for those fixtures and direct transport assertions before editing. Do not weaken unrelated assertion suites to make tests green.

**Acceptance gates:**
- [ ] For deleted, inactive, and expired proxies (including `FallbackModeNone` and unresolved fallback), scheduler does not select the account; candidate check has been exercised through Redis meta `GetSnapshot`, not a hand-built Account.
- [ ] Direct invocation refuses/fails over and fake transport proves zero direct outbound requests; resolver/repository lookup errors are fail-closed too.
- [ ] Status/expiry/delete/restore mutation refreshes all live accounts whose own `proxy_id` matches the changed proxy (including shadows) within the configured outbox poll bound; tests verify event-driven update rather than waiting for full rebuild.
- [ ] No-proxy accounts still route direct; active/unexpired proxies still use the configured proxy.
- [ ] Shadow request and candidate decisions use the shadow row’s own synchronized `ProxyID` and `Proxy`; a proxy update test proves `propagateProxyToShadows` keeps the parent and shadow IDs equal.
- [ ] Custom base URL retains relay parameter behavior and still rejects invalid configured proxies before relay request construction.
- [ ] A privacy-safe log/ops event records only account ID, proxy ID and reason for rejected/unselected accounts; no URL, host, port or credentials.
- [ ] Exact grep `grep -rnE '\.Proxy != nil' backend/internal/service --include='*.go' | grep -v _test.go` returns exactly **1** line, the documented centralized `resolveAccountProxyURL` whitelist entry. Any other remaining hit fails acceptance.
- [ ] After initial rebuild, every ID referenced by an active candidate bucket resolves through `GetSnapshot` to meta carrying `ProxyID` and (when bound and present) minimal proxy status/expiry. No active bucket continues to reference legacy-format meta.
- [ ] Run all affected package tests with `-tags=unit -count=1`; run the full backend unit suite once at final Phase 1 tree. Record command, exit status, test counts, and tree hash.
- [ ] Run the count-only Q7 SQL before deployment and report only the integer.

### Test inventory and anticipated assertion changes

- Existing `IsSchedulable` unit cases should not change because this plan explicitly avoids widening `IsSchedulable()` to depend on a proxy relation.
- Existing proxy expiry integration cases in `backend/internal/repository/proxy_expiry_integration_test.go`, `proxy_repeated_expiry_integration_test.go`, `proxy_inactive_backup_integration_test.go`, and `proxy_restore_probe_integration_test.go` should retain persistence/fallback assertions. Add scheduler/request behavior tests beside them; alter an existing assertion only if it currently expects outbound use/direct fallback after a proxy becomes unavailable, and cite that exact function in the implementation review.
- R6 maps each production grep hit to an error-propagation batch and targeted test command. Before coding, inventory exact existing test names in those suites; only assertions that explicitly expect invalid configured-proxy requests to reach direct transport may change. Report each changed assertion by test function and explain why; do not alter unrelated expectations.
- Every acceptance regression, especially the Redis marshal→write→`GetSnapshot` test and no-direct-request tests, is first run against untouched `ca0946e91`. The implementation review packet must paste exact command/output for each red regression and later record green results; this plan does not claim any unexecuted test has already failed.

---

## Explicitly deferred: Phase 2 / G4

Do not add probe health to account scheduling in this phase. A future independent plan must decide threshold/recovery and shared probe-outage protections. The production deployment on 2026-10-06 reported 60 matching probe-failure log lines in ten minutes; this reinforces keeping VPN Gate/pool-managed proxies out of Phase 1 status gating and handling probe health separately with no mass account disablement when the probe service itself is broken.

## Review boundary

This is a design/implementation plan. The reviewer’s conditional approval is recorded above, and the S1/S2 conditions are now incorporated; no production code, tests, UI, docs, schema, or configuration have yet been changed for implementation. Phase 1 and Phase 2 must be separate changesets; any commit, push, or PR requires the user’s separate explicit authorization.
