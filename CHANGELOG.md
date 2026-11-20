# 更新日志

条目用中文，按「新增功能 / 优化改进 / Bug 修复 / 破坏性变更」归类，写用户能感知的变化，格式与原版 sub2api 的发布说明一致。


## Unreleased

## Main / Portal 0.2.82 — 2026-11-20

发布期间管理后台不再整段不可用；DeepSeek 关闭联网工具时的请求处理改正；Portal 的配置校验和关闭流程更稳。

### 优化改进

- 发布时旧后台在排空期间继续提供管理页面和管理读接口，只拒绝写请求和少数需要访问上游的读请求，返回带原因的 503 和 `Retry-After`。管理接口只在旧后台退出到新后台就绪之间短暂不可用，不再整段排空期都不可用；`/health` 全程返回 200。此前打开着运维实时页会让旧后台一直退不出去。
- 发布工具在后台交接期间每 0.5 秒轮询一次，在发出停止信号前说明管理接口会变成只读还是不可用，并显示旧后台还在停止哪些服务。旧后台超出等待时间仍未退出时按失败结束，保留进行中状态、当前提供服务的槽位和人工处理说明；`status` 显示各容器的重启策略。
- Portal 在打开状态之前校验配置，普通启动和 `-check-config` 报告同一套配置错误；工作台和画布要求 HTTPS，来源按规范化后的协议、主机和端口比较，拒绝有歧义或旧式写法的地址。
- Portal 在关闭时取消只读的余额查询，并记录进行中的请求、待完成的后台任务、当前检测轮次和排空耗时。被关闭打断的读请求保留原有余额记录、计划与告警状态，不记为失败；慢的同步刷新不再拖住交接。
- DeepSeek 联网工具识别供应商自己的工具类型：`deepseek_web_search` 按 `web_search`、`deepseek_read_url` 按 `web_fetch` 处理，三种协议下都由网关执行并计费，包括 `tool_choice` 强制指定的情况。
- 移除 DeepSeek 账号在原生 Responses 协议上未使用的工具分支及其账号切换。

### Bug 修复

- 关闭联网工具的 DeepSeek 分组里，网关的工具循环不接管请求时，供应商自己的两个工具类型在转发前移除并附上实时信息不可用的提示。此前上游会执行没有计费的付费工具调用。其余字段按原始字节转发。
- 客户端自己声明联网工具的请求，在工具无法建立时不再被直接拒绝：移除联网工具和工具调用上限，告诉模型无法核对实时信息，按普通请求回答和计费。

## Main / Portal 0.2.81 — 2026-11-20

- Portal: administrators can register workbench and canvas groups from the page. Ticking a group fetches its models to pick from, and a saved change applies to the next request, with no edit of the registry file and no restart. Media engine groups are still configured only in the registry file.
- Portal: a user whose group is not registered can connect the workbench and the canvas with one of their own API keys. The host is always the site; the URL prefix can be changed and starts from the one served for the key's platform; models are fetched under it and picked. Only the key's ID is stored, and administrators can turn the feature off in the group settings.
- The release tool can deploy the Portal: `deploy.py portal <image>`, `portal-rollback`, `portal-status` and the one-time `portal-adopt`, with settings in `/opt/sources-deploy/portal.json` (Main commands keep their default config). Before the old Portal is signalled it checks the host, Nginx and files, runs the new image's read-only `-check-config` start-up check (new in the Portal; it validates the environment and on-disk state without taking the lock) and creates the new container. The old Portal then gets one SIGTERM and is never killed; the new one starts as soon as it has exited, and Nginx switches once its `/health` answers, so the pages are unavailable only for that short handover. An old Portal that takes longer than expected is waited for with a warning every 30 seconds after 420 seconds; only after an hour does the tool stop, without killing anything, and leave the restart to an operator. A failure after the signal, a command timeout or an interrupt stops the new container, restarts the old one and switches Nginx back; a guard watches the new Portal for 60 seconds. A rollback also works when the current Portal has crashed. Portal and Main releases share one release lock.

- DeepSeek groups can offer five standalone tool endpoints, `POST /v1/tools/search`, `/fetch`, `/ocr`, `/asr` and `/tts`, relayed to the tool account's supplier and billed from the balance at fixed unit prices that are not multiplied by the group rate: OCR per recognized page, ASR per second (rounded up, priced per hour), TTS per input character with a Han character counting as 2 (priced per 10,000). The three new prices live in the group's tool settings; a tool without a price is not offered, and existing Search/Fetch prices keep their meaning. No migration.
- Only units confirmed by a successful supplier answer are charged; a failed, empty or error answer is not. ASR is charged by the audio's length, so silent audio with an empty transcript is charged too. Size, type and length limits are checked and the estimate is reserved before anything is sent; for OCR anything but a single image is reserved at the 1,000-page maximum, and ASR is charged by the duration the supplier reports, not the one the file declares. A request moves to another tool account of the group only when the supplier refused it before anything was relayed; a request whose outcome is unknown is never sent again. OCR and ASR finish and are billed after a client disconnect; TTS stops with the client.
- The standalone tool endpoints choose among the group's tool-enabled DeepSeek accounts through the scheduler, and a pinned tool account still wins; in-chat tools keep using the model account or the pinned one. Supplier costs on tool accounts are optional and count as 0 in profit statistics when missing.

## Main / background 0.2.80 — 2026-11-20

- Record the upstream response status and request identifiers (`X-Client-Request-ID`, `X-Request-ID`, `CF-Ray` and the header named by the account's `upstream_request_id_header`) in the content-rejection ledger for holds that end unknown or are released without charge, in addition to charged rejections, and keep them through the deadline release, so a hold can be matched to one supplier bill line. The recorded status is the one the response carried, never one the gateway made up for an error. They are evidence for reconciliation and never decide a charge. No migration; commands with nothing observed keep their earlier fingerprints.
- A response header can no longer make a rejection settlement fail: a request id with bytes PostgreSQL cannot store (a control character, invalid UTF-8) is dropped instead of failing the write. Before, such a value in the account's request-id header left a charged rejection uncaptured until its hold was released.
- Add the setting `upstream_rejection_deadline_release_paused`: `true` pauses the deadline release of content-rejection holds and keeps them frozen; a missing key or `false` keeps the 0.2.79 behaviour; any other value, an empty one included, pauses. It is read before every release, so setting it also stops a run that is under way. The release log line reports the longest overdue time of a run. The summary of a run and every change of the pause switch are logged as warnings, so they are kept by a deployment that logs at warn and above; a run in which a release failed is logged as an error.
- In a group that charges the content-rejection fee, a composer chat request with image input is refused with a 400 before anything is sent upstream, and its hold is released at once without charge. Before, the image-description request used up the one send a hold allows, so the request itself was never sent and the hold stayed frozen until its deadline. Groups that do not charge the fee still describe the images first.
- Turn DeepSeek thinking off for a request that forces a tool call (`tool_choice` `required` or a named function). DeepSeek rejects a forced choice while thinking is on, so these requests failed with 400; `auto`, `none` and requests without a `tool_choice` are forwarded unchanged.
- Keep DeepSeek requests working when the optional web tools cannot be used: a group with the tools off serves requests that carry a `web_search` or `web_fetch` tool on the ordinary path, and tools the gateway adds on its own no longer fail the request when the tool account is not ready. A request that names a server tool itself still gets the error.

- The release tool (`deploy/sources/deploy.py`) sends an explicit `User-Agent` with every request, and its preflight requests the public health endpoint exactly as the guard does, stopping with nothing changed when it does not answer 200 (also in `--dry-run`). Before, edge protection in front of the public endpoint rejected the default urllib identity, so the guard rolled back a healthy release. `main --allow-public-health-failure` turns the preflight check into a warning for rolling forward out of an outage; the guard still requires 200, `rollback` is never blocked by it, and a guard failure names the last status.
- A failed dry run of the release tool, or a failed dry-run rollback, no longer appends a `failed` record to `history.jsonl`.

## Main / background 0.2.79 — 2026-11-19

- Sync official Sub2API v0.2.23: GPT-6.2 Sol and Astra (aliases, Codex model metadata, catalogue prices and long-context rates), an optional Ultra Fast multiplier on channel price cards (migration 252 is idempotent with the existing column), cache-creation tokens derived from the 5m/1h breakdown when the total is missing, an OpenAI pre-output idle failover, and the related upstream fixes and tests.
- Keep the existing price of GPT-6.2 price cards whose Ultra Fast value is empty: 6x at every date instead of upstream's 3x before 2027-03-01 08:00 UTC. A value set on the card still wins; requests without a card follow upstream.
- Bill GPT-6.2 requests that have no price card at upstream's catalogue rates instead of recording zero cost.
- Show the channel Ultra Fast field only for OpenAI channels whose models all support Ultra Fast.
- The upstream baseline pin records v0.2.23 with its linux/amd64 image digest.

- Let the background process finish in-flight OAuth token refreshes and save the new credentials on shutdown instead of cancelling them; channel probes and scheduled tests stop at once without recording false failures.
- Add the one-shot commands `-list-pending-migrations`, `-check-startup` and `-export-web-assets` for the fixed-slot release tool in `deploy/sources/`.
- Reject destructive migrations in CI unless they are explicitly marked as `contract` migrations.
- Release content-rejection fee holds of synchronous requests whose outcome is still unknown after a durable deadline (migrations 272 and 273, the index built concurrently). A late rejection is recorded and never charged; a late ordinary bill is recorded and not charged after a release from `unknown`, and charged once through the ordinary path after a release from `sent`. Task-bound holds and holds already waiting before this change are unchanged.
- A dry run of the release tool (`deploy.py --dry-run main <image>`) skips the startup check while migrations are pending and says so, instead of reporting a failure a real run would not have: a dry run only prints the migration command.

## Main / background 0.2.78 — 2026-11-18

- Keep Fast and Ultrafast multiplier controls available for GPT-6.2 Sol/Astra, including mixed pricing entries with existing supported models.
- Align model aliases, price-card validation, request-tier selection and billing defaults; preserve standard input/output/cache prices, explicit overrides and empty-value inheritance.
- Cover native group form saves, clearing overrides, HTTP/WebSocket tier handling and billing with targeted regressions; no schema or upstream baseline change.

## Main / background 0.2.77 — 2026-11-18

- Preserve atomic image settlement and release through the billing-lane wrapper so a nonempty media engine registry can initialize with the native provider wiring.
- Forward the user pending-rejection reader through the same wrapper, preserving original arguments, context and repository outcomes without entering the batch lane.
- Write fatal startup errors directly to stderr before exit, including when runtime logging is set to WARN.
- Cover configured media engine initialization and fatal-error visibility with targeted regressions. No new migration.

## Main / background 0.2.76 — 2026-11-17

- Integrate official Sub2API v0.2.22 while preserving Sources billing, wallets, image recovery, channel cards and private historical usage.
- Let administrators explicitly enable per-Key Fast / Ultrafast selection for a group and assign each fixed supplier account one tier; ordinary groups keep their existing behavior.
- Route, retry and resume only within the selected tier, reject stale queued/WebSocket configuration, and retain the selected tier in one immutable billing snapshot.
- Preserve private billing extensions through the native batch lane and shut down settlement after usage drains; keep archived totals with nonblocking rollups.
- Apply four additive migrations without rewriting prior migration checksums.

## Main / background 0.2.75 — 2026-11-17

- Show V2 channel health as responsive provider cards with status bars, user rates, cache rate, availability and first-token latency; preserve V1.
- Let administrators sort categories and channels, override display categories and choose separate or provider-based image groups with atomic, versioned settings.
- Keep diagnostic controls private to administrators, distinguish missing samples from zero values, and discard stale privileged data when the viewer changes.
- Preserve the official v0.2.21 baseline, existing billing and data; no database migration.

## Portal 0.2.74 — 2026-11-17

- Expose registered native Gemini image groups in Studio and Canvas with the generateContent protocol and supported image controls.
- Preserve ordinary group-key ownership and reuse, and retain the existing media engine asynchronous task contract.

## Portal 0.2.73 — 2026-11-16

- Accept existing intelligence-test keys with unlimited cumulative quota and no expiry while retaining a positive daily spending limit and native ownership checks.
- Refresh the key snapshot on ordinary plan saves, and allow pausing a plan after its key or group becomes unavailable.
- Preserve active submissions across pause and restart without resubmitting them; retain existing keys, usage and test history.

## Main / background 0.2.72 — 2026-11-13

- Update the official baseline from v0.2.19 to v0.2.21, including request-memory budgets, stream/failover fixes, MCP, model compatibility, and 30m/12h cache-write accounting.
- Preserve Sources wallets, image holds, DeepSeek recovery, fixed rejection fees, Fast/Ultrafast pricing, branding, and historical billing snapshots.
- Route management MCP to the sole background owner and drain usage before stopping balance notifications.

## Main 0.2.71 — 2026-11-11

- Continue answering without hosted web tools when Search/Fetch fails; preserve client functions and disclose unavailable live verification.
- Allow one tool-free attempt after a confirmed first-turn HTTP 500/502/503 rejection before output or usage, without replaying unknown transport outcomes or paid work.
- Preserve upstream diagnostics and distinguish model unavailability from interruption after confirmed usage.

## Main 0.2.70 — 2026-11-11

- Resolve current local API-key billing groups in the admin channel-monitor list so status clients automatically follow new monitors, key replacements and group changes.
- Return an explicit unavailable association for unbound/deleted keys or groups and lookup failures, without exposing credentials or changing usage accounting.

## Main 0.2.69 — 2026-11-09

- Preserve real streaming during DeepSeek hosted Search/Fetch across Chat, Responses and Messages. Forward content immediately while keeping private tool calls and intermediate terminals internal.
- Measure first output at the public stream writer and leave non-streaming first-token timing unset; retain confirmed multi-turn usage and stop new tools after disconnects.
- Keep pricing, ledger and database schema unchanged.

## Main 0.2.68 — 2026-11-09

- Preserve verified wallet bindings, balance configuration and snapshots when only model mappings or capability metadata change.
- Reject stale refresh claims without clearing the current snapshot; retain identity validation for credentials, URLs and proxies.
- No database migration.

## Main / Portal 0.2.67 — 2026-11-08

- Clarify missing New-Api-User guidance when saving upstream balance sources.
- Recover and close uncertain image tasks using their original holds and bounded policy, without replaying upstream generation.
- Preserve native settlement idempotency and expose image recovery in operations.

## Main 0.2.66 — 2026-11-07

- Keep injected DeepSeek Search/Fetch function names stable across requests so unchanged tool definitions preserve reusable prompt prefixes.
- Resolve customer tool-name collisions deterministically while retaining per-request ownership, confirmed-usage accounting and existing tool prices.
- Retain the 0.2.65 backup implementation.

## 0.2.64 — 2026-11-06

- Add opt-in DeepSeek Search and Fetch with group prices, shared account credentials and per-key automatic tool settings.
- Execute built-in tools through Responses, Chat Completions and Messages, and expose standalone Search/Fetch endpoints with separate successful-call fees.
- Preserve confirmed model/tool usage in durable checkpoints and settle balances, key budgets, supplier quotas and receipts atomically without replaying upstream work.
- Retain the native OpenAI routing restoration and decompression security fixes from 0.2.63. Compatibility tool streaming buffers the final answer; supported native Pro streaming remains live.

## Main 0.2.63 — 2026-11-06

- Remove the custom scoped OpenAI session affinity settings, bounded original-account queue and encrypted-history provenance layer.
- Restore the pre-enhancement native scheduling, sticky sessions and history recovery paths on the pinned Sub2API v0.2.19 baseline.
- Retain existing billing, wallets, account configuration and historical data; no database migration is added.
- Update Brotli to v1.2.7 and klauspost/compress to v1.20.2 to fix the reachable decompression allocation and panic vulnerabilities reported by the release security scan.

## Portal 0.2.64 — 2026-11-06

- Add administrator deletion of intelligence test plans with explicit confirmation and revision checks.
- Back up monitoring configuration before removal, retain native groups, keys and billing records, and protect unfinished tests and paid-request deduplication.
- Remove deleted plans from both test galleries and prevent stale scheduler snapshots from recreating them.

## 0.2.62 — 2026-11-05

- Synchronize the official Sub2API v0.2.19 baseline while retaining Sources billing, wallet and lifecycle boundaries.
- Add opt-in, explicitly scoped Codex/Responses session affinity with a maximum one-second original-account queue budget and atomic route ownership.
- Protect encrypted history across HTTP, SSE and WebSocket using tenant-, group- and upstream-identity-qualified provenance; preserve known usage when local tracking fails.
- Keep disabled and unselected groups on the existing routing and error-accounting paths.
- Expose observation targets and recorded cache statistics without changing token counts, pricing or balances.


## 0.2.61 — 2026-11-03

- Add optional fixed content-restriction fees for eligible Grok balance groups, independent of normal usage charges and pricing multipliers.
- Reserve balances and API key budgets before requests; settle each request once and retain unresolved charges for reconciliation across streaming and asynchronous media requests.
- Show concise fee disclosures and private pending-charge records without exposing provider details or internal billing rules.
- Import canonical Grok model names without prefixed aliases for compatible accounts.

## 0.2.60 — 2026-11-02

- Allow manual and scheduled database backups to complete within a two-hour total deadline, including dump, compression and multipart upload.
- Keep the scheduled backup lease and interrupted-backup cleanup window beyond the full deadline, while preserving the existing restore and record-update limits.
- Update affected HTML parsing, translation and URL dependencies to their minimum security-fixed versions.

## 0.2.46 — 2026-10-19

- Restore group Fast/Ultrafast multiplier controls by fixing the Vue parent-to-child prop binding; add a full group edit/save regression test.
- Hide the legacy force-Fast description when an explicit tier policy is selected. Backend pricing and Standard unit prices remain unchanged.

## 0.2.45 — 2026-10-19

- Reject older authentication snapshots that cannot preserve group tier policies or Ultrafast overrides during mixed-version rollout.
- Retain the scoped Fast and Ultrafast pricing, immutable billing evidence and unchanged Standard unit prices from 0.2.44.

## 0.2.44 — 2026-10-19 (superseded by 0.2.45)

- Preserve standard token prices while allowing group Fast/Ultrafast multipliers and API-credit or subscription-allowance retail presets; correct GPT-6.1 Sol Ultrafast to 6×.
- Add opt-in group tier selection with explicit rejection of unsupported or conflicting forced-Ultrafast requests across HTTP and WebSocket paths.
- Persist per-request tier and price evidence without recalculating historical charges; preserve free-Fast, upstream downgrade and duplicate-billing behavior.
- Preserve model price cards when copying groups.

## Portal 0.2.43 — 2026-10-18

- Default new intelligence plans to administrator-only visibility; protect both snapshots and result links and preserve existing shared plans during migration.
- Keep legacy audience fields private so older Portal readers cannot silently expose private tests on rollback.
- Reuse native exclusive groups and scoped keys, provide links to existing account/group configuration, and clarify record reset while preserving active tasks, limits and rotation.

## Portal 0.2.42 — 2026-10-18

- Allow authenticated read-only intelligence galleries with server-enforced administrator writes, explicit display fields and credential checks. Keep legacy private storage for safe rollback.
- Back up and clear selected terminal history while retaining live tasks, paid-request deduplication and original keys.
- Load artwork with six coalesced reads and a bounded cache, spread preview initialization across frames, and suspend hidden previews without capping animation frame rate or resolution.

## Portal 0.2.41 — 2026-10-17

- Open creative results in the same isolated, complete-canvas viewer as pelican results, including fit and native-size modes.

## Portal 0.2.40 — 2026-10-17

- Rotate each enabled drawing plan through four fixed pelican tests and one creative test, persisting its position without extra submissions or new keys.
- Label and filter both artwork types while retaining the existing cadence, limits and complete-canvas preview. Main and media artifacts are unchanged.

## 0.2.39 — 2026-10-17

- Add administrator-only intelligence monitoring in the existing Portal, with durable asynchronous candy and pelican tests, native scoped key limits and configurable schedules.
- Share Main appearance preferences without exposing credentials in embedded links; show complete artwork by default with optional native-size scrolling.


## 0.2.38 — 2026-10-16

- Restore background image generation and reference editing through the existing durable task queue and working native image endpoints; retain task lookup, original billing and legacy remote-task recovery.
- Align embedded Studio authorization with native session policy and keep provider details out of public image errors.


## 0.2.37 — 2026-10-16

- Preserve administrator historical usage, model distribution and user rankings when old usage details are removed. Add compact statistic sub-buckets with exact timezone-boundary checks; billing data and financial references remain protected.
- Add date-scoped, unarchived maintenance batches with atomic statistics and receipts and explicit manual scheduling.

## 0.2.36 — 2026-10-15

- Add a default-off external log-retention owner switch for usage records, operational logs and their retained statistics. Refuse controlled deletion when ownership cannot be verified.
- Reject competing native usage-cleanup requests while external maintenance owns retention. Preserve aggregation, billing deduplication, audit retention and other background maintenance.

## 0.2.35 — 2026-10-15

- Restore compact colored upstream balance badges and the legacy display precision: whole amounts at magnitude 100 or above, two decimals below. Keep exact balances available in the tooltip.
- Display “无限额度” for balances strictly above 1,000,000 without changing stored money or hiding historical-value and failed-query indicators.

## 0.2.34 — 2026-10-15

- Read upstream Sub2API wallet balances automatically with each account’s existing endpoint, API key, proxy and transport configuration. Keep existing manual balance sources unchanged and offer manual setup only for exceptional cases.
- Display only explicit wallet balances; subscription and API-key limits are not presented as account money. Preserve zero balances, exact decimals and same-account historical values on failures.
- Bound concurrent read-only queries and cache results; reject stale responses after account credentials change. No financial schema, model billing or background task changes.

## 0.2.33 — 2026-10-14

- Allow repeated media admission holds for independent background deployments. A new hold waits for in-flight submissions and refuses to hide an unfinished previous batch; stale release requests remain rejected.
- Keep serving instances controllable after the legacy origin is retired, using the existing durable journal and unique background ownership. No financial schema or billing changes.

## 0.2.32 — 2026-10-14

- Add an upstream wallet balance column and configuration drawer to account management, with shared sources, explicit currencies, successful-query timestamps and stale-value indicators.
- Reuse encrypted Portal sources and caching; add separate account associations and a bounded administrator identity API so changing calling credentials or endpoints requires reconfirmation.
- Refresh visible wallets once per source, retain successful values on query failure, and preserve native login renewal. Old balance links redirect to account management.
- Retain the official Use Key and CCS import restoration from 0.2.31. No Main financial schema, recharge or model billing changes.

## 0.2.31 — 2026-10-14

- Restore the upstream Use Key and Import to CC Switch entries, including the native client dialogs and the administrator's hide-CCS setting.
- Remove the private policy that incorrectly hid these upstream functions. Legacy private import wizards and protocols remain removed.

This patch does not change the gateway, billing, migrations or background ownership.

## 0.2.30 — 2026-10-14

- Separate Main request serving from unique background ownership; preserve live concurrency slots and let existing HTTP, streaming, image and WebSocket work finish during planned changes.
- Add a guarded first upgrade from 0.2.28, with durable media admission state, exact process identities and no return to a generation that has received TERM.
- Preserve Portal state ownership and accepted responses during handoff; recover embedded login sessions and provide a separate prefixed Canvas entry while retaining the existing Studio.
- Include the V1 monitor ordering, cache-only usage column and original palette accents from the immutable 0.2.29 artifacts.

## 0.2.29 — 2026-10-13

- Restore V1 channel monitor ordering with drag and move controls; admin and user lists share the saved order, and existing positions survive migration.
- Keep output TPS in the native latency column; simplify the separate performance column to cache hit rate.
- Restore original palette accents for usernames, selected usage tabs and icons, including the original golden yellow; keep readable primary button text.

## 0.2.28 — 2026-10-13

- Sync official Sub2API v0.2.16 at 01842576b3d5c1e2545b5a99ad555d36e28ed306, including Haiku 5.5, Gemini explicit cache billing and recovery, Vertex improvements and localized groups.
- Preserve audited media engine reservation/settlement, verified native USDT payments, full mobile network labels, eight themes and the accepted homepage colors.
- Bind builds, image verification and runtime comparisons to one immutable upstream identity; preserve historical migration checksums and fixed 0.2.27 artifacts.
- Verify cache recovery, media billing, old serving coexistence, graceful stream routing and 0.2.26 application rollback after new writes on an isolated restored database.

## 0.2.27 — 2026-10-13

- Rebase the product on fixed official Sub2API v0.2.15, preserving existing business data.
- Keep eight local theme palettes, the independent homepage/editor, welfare navigation, native-key image workbench, 20/60-second monitoring and performance column.
- Isolate media engine asynchronous tasks and upstream personal-token balance monitoring from the core database.
- Preserve native BEpusdt checkout with verified payments, unique receipts and durable reconciliation.
- Build releases from an immutable source tree with recorded CI evidence.
