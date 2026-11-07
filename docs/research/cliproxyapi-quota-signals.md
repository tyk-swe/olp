# CLIProxyAPI quota signals and what OLP could adopt

Research snapshot, 2026-11-05. Pinned sources are [CLIProxyAPI at 585f97d](https://github.com/router-for-me/CLIProxyAPI/tree/585f97deac9aa57c700301221fb1a7085e1827cc) and [Codex at c0100ad](https://github.com/openai/codex/tree/c0100ad4ecd2251c2b48b68534db08afac78c606). Vendor plan details describe that date. Here, proven means stated by vendor documentation or client source, not OLP live qualification. Heuristic means parsing or classification found only in CLIProxyAPI.

CLIProxyAPI does not rank accounts by remaining allowance. It displays passive observations, applies failure cooldowns and selects within a priority tier. Resets govern eligibility and waits between retries. Allowance-based ordering would be new OLP work, governed by [ADR 0005](../adr/0005-make-code-mode-better-than-magpie.md).

## CLIProxyAPI selection

The [selector][selector] excludes disabled or expired credentials, live credential-wide quota blocks and unavailable or exhausted per-model state. It keeps the highest eligible `priority` tier, sorted by credential ID, then applies round-robin, smooth weighted round-robin or fill-first. Weights default to 1, cap at 1,000,000 and exclude nonpositive values. The [scheduler][scheduler] mirrors this with provider/model/priority ready and blocked views, promoting expired blocks on read. [Configuration][routing-config] documents the strategies.

Optional session affinity outranks priority while its credential remains available, with a default one-hour lifetime. `session-affinity-rebind-on` can rebind on any failure or only quota/auth failures, borrowing another credential for transient failures. Codex websocket requests prefer websocket-capable credentials. A [plugin scheduler][selection] can replace built-in selection.

No built-in selector or scheduler reads quota snapshots, remaining percentages or resets to order ready accounts. If every model candidate is cooling down, selection returns synthetic 429 with Retry-After for the earliest recovery. Fill-first reserves unused accounts and weights encode static plan shares, but neither measures remaining allowance.

## Observations and active reads

[`QuotaState` and `ModelState`][quota-state] separate scheduling fields `Exceeded`, `Reason`, `NextRecoverAt` and `BackoffLevel` from `ObservedAt` and a bounded `Signals` snapshot. [Passive observation][quota-signals] replaces rather than merges each response snapshot and never changes cooldown state. Supported providers are Claude, Codex and Devin.

| Provider | Retained signals |
| --- | --- |
| Codex | Retry-After, plan type, active limit, credits and `x-codex-` fields containing `-allowed`, `-limit-reached`, `-limit-name`, `-used-percent`, `-window-minutes`, `-reset-after-seconds`, `-reset-at` or `-over-secondary-limit-percent`. |
| Claude | Retry-After and `anthropic-ratelimit-unified-` headers. |

Snapshots cap at 64 headers and 512 bytes per value, preserving credential watermarks before additional namespaces. [Codex websocket events][codex-events] normalize `codex.rate_limits` primary, secondary, additional and code-review limits, credits, plan and active-limit names to headers. The `/wham/usage` reference in that file is only a comment, not an active Codex fetch.

[Devin user status][devin-status] is the sole built-in active reader, returning daily and weekly percentages and resets. Other reads require a [quota-provider plugin][quota-plugin] or an operator-defined [management `quota_probe`][quota-probe] with a `$TOKEN$` placeholder and optional response mapping. Plugin buckets carry window name, remaining fraction and reset. No quota plugin for OLP's Codex, GLM, MiniMax, Kimi, Qwen or OpenCode Go adapters ships in the pinned tree.

[Management listings][auth-listing] expose observations while stripping cooldown fields. [Manual quota reset][cooldown] clears local routing cooldown, without changing upstream allowance.

## Cooldowns and retries

[`Manager.MarkResult`][cooldown] records execution outcome, model, retry hint and whether the executor declares credential-wide scope. A credential-wide 429 blocks every model while preserving earlier model blocks separately. Ordinary model failures affect only that model. Live deadlines never shorten, and a success dispatched before a failure cannot clear its cooldown because execution sequence watermarks fence it.

| Failure | Cooldown |
| --- | --- |
| 429 with upstream hint | Hint with a ten-second floor. |
| 429 without hint | Exponential ladder from one second to 30 minutes. Concurrent failures reuse the open window rather than advancing repeatedly. |
| 401, 402, 403 | 30 minutes. |
| 404 | 12 hours unless a hint overrides it. |
| 408, 500, 502, 503, 504, 520–526 | Hint, otherwise one minute by default. `transient-error-cooldown-seconds` zero retains that default, negative disables transient cooldown. |

Cooldown is in memory unless `save-cooldown-status` persists `.cds` files. `disable-cooling` works globally, per provider or per credential. Sources are the [cooldown implementation][cooldown], [ladder constants][cooldown-constants] and [configuration][cooldown-config].

[Codex error handling][codex-errors] treats 429 `usage_limit_reached` as credential-wide, reading `resets_at` before heuristic `resets_in_seconds`. Its capacity classifiers are model-scoped heuristics. [Claude handling][claude-resets] takes the latest Retry-After or rejected unified five-hour, seven-day or combined reset, plus 1–30 seconds of grace, distinguishing shared-window rejection from overage-only rejection. [Generic OpenAI-compatible handling][compat-retry] honors standard Retry-After, with a one-minute fallback only for explicit per-minute token-limit wording.

The [execution loop][execution] tries untried credentials within each round. A 401 first gets one refresh and reexecution on the same credential. Additional rounds admit 403, 408, 429, 500, 502, 503, 504 and transient transport errors. `request-retry` defaults to three additional rounds, overridden per provider or credential. Zero allows only the first round. Each later round admits only credentials whose retry budget covers it. [Retry configuration][retry-config] and [selection helpers][selection] define those limits.

Between rounds, the earliest eligible cooldown determines the wait. A just-failed 429 credential has a ten-second minimum. `max-retry-interval` defaults to 30 seconds, with nonpositive values disabling waits. Wait jitter adds up to two seconds. `max-retry-credentials` zero tries all available credentials. [Stream bootstrap][stream-bootstrap] buffers opening chunks, with `bootstrap-retries` controlling retries before the first client byte. That boundary is weaker than OLP's required proof of non-execution under [ADR 0002](../adr/0002-state-aware-code-account-recovery.md).

## Vendor signals

### Codex and ChatGPT subscription

The fetched [pricing page][codex-pricing] described five-hour estimates for Plus and Standard Business, no five-hour limit for Pro, possible weekly limits, dashboard resets and CLI `/status`. Those plan details are dated observations.

| Signal | Evidence |
| --- | --- |
| GET `{base}/wham/usage` for ChatGPT or `{base}/api/codex/usage` for Codex API | Proven by the [official usage URL builder][codex-usage]. CLIProxyAPI does not call it. |
| `x-{limit}-primary-used-percent`, `-window-minutes`, `-reset-at`, with secondary counterparts | Proven by the [official header parser][codex-limits]. |
| `x-codex-credits-has-credits`, `-unlimited`, `-balance` | Proven by the [credits parser][codex-credits]. Keep credits separate from included allowance. |
| `codex.rate_limits` windows with `used_percent`, `window_minutes`, `reset_at` | Proven by the [official event parser][codex-limit-events]. |
| 429 `usage_limit_reached` with epoch `resets_at` and `limit_window_minutes` | Proven by the [official error path][codex-429]. |
| Event `reset_after_seconds`, error `resets_in_seconds` | Heuristic spellings in [CLIProxyAPI events][codex-events] and [errors][codex-errors]. |
| `turn_rate_limit_exceeded` or model-at-capacity wording as transient capacity | Heuristic [CLIProxyAPI classification][codex-errors]. |

The [paid API rate-limit headers][openai-limits], including request/token limit, remaining and reset fields plus Retry-After, describe organization/project limits and do not establish subscription signals.

### GLM Coding Plan

The [plan overview][glm-plan] documents five-hour credits resetting five hours after consumption and weekly credits resetting every seven days from activation. The [FAQ][glm-faq] offers console progress, while the fetched [documentation index][glm-index] supplied no remaining-allowance endpoint or response header.

[Errors][glm-errors] have HTTP status plus JSON `code` and `message`. Codes 1302 and 1305 indicate short rate/capacity failures. Codes 1308, 1310 and 1316–1321 indicate usage windows, with `{next_flush_time}` substituted into several message templates. Code 1311 is model-scoped entitlement, while 1309 and 1314 mean expired package. CLIProxyAPI has no parser for these codes, leaving generic Retry-After and cooldown behavior.

An OLP classifier would need complete JSON bodies and exact documented templates before accepting an unambiguous reset timestamp. Loose date extraction would turn prose into an admission gate. No machine-readable remaining value was found, so fresh ordering cannot be inferred. Existing weights can reflect documented weekly plan-credit ratios as static shares.

### MiniMax M Plan

[Usage rules][mm-usage] document independent five-hour and weekly windows for non-video models, anchored at first use and fully restored at each window end. The [FAQ][mm-faq] documents bearer remains reads and Explore/Build ratios of 3 and 7.5 relative to Go. These ratios can inform static weights, not current allowance. [`mmx quota`][mm-cli] is a supported usage surface.

The [vendor catalog][mm-errors] lists usage codes 2056 and 2067, ambiguous 2062, general rate limit 1002 and token limit 1039. The fetched usage, FAQ and error pages document no inference quota/reset header. CLIProxyAPI has neither a remains client nor a MiniMax code classifier.

The [consolidated MiniMax evidence](minimax-kimi-vendor-clients.md#minimax-response-fields-and-interpretation) records client schema, count ambiguity and display behavior. Its [replay](minimax-kimi-vendor-clients.md#inference-replay-remains-unproven) and [regional-fixture](minimax-kimi-vendor-clients.md#regional-remains-fixtures-remain-unproven) findings keep inference non-execution and real-key remains qualification unresolved. Catalog codes alone permit no replay, and client fixtures permit no allowance ordering.

### Kimi Code and Moonshot

[Kimi membership][kimi-membership] describes rolling five-hour limits and shared monthly quota, with weekly limits on legacy plans anchored to subscription date. Console and CLI `/usage` show usage. [Documented 429 messages][kimi-errors] distinguish five-hour, weekly and monthly quota from concurrency, request rate and engine overload, without reset timestamps. No subscription API-key remaining endpoint or quota header appears in those pages.

The [vendor client's managed usage reader](minimax-kimi-vendor-clients.md#kimi-managed-usage) is separate evidence, using bearer auth at `/usages`. It does not establish acceptance for OLP's subscription API-key profile. Its [chat decoder](minimax-kimi-vendor-clients.md#kimi-chat-errors-and-retries) uses structured quota codes, without a window-message classifier. CLIProxyAPI's [native executor][kimi-executor] forwards status and body without Kimi-specific quota or retry-hint parsing.

Extra Usage can continue spending after included quota is exhausted. Enrollment should distinguish that from spare subscription capacity. The paid Moonshot [balance endpoint][moonshot-balance] returns `available_balance`, `voucher_balance` and `cash_balance`, while its [limits][moonshot-limits] describe concurrency, RPM, TPM and TPD, with rate-limit headers only for web search. Neither proves Kimi Code subscription allowance. Do not invent resets from unknown subscription dates.

### Qwen Token Plan

The [Token Plan FAQ][qwen-token] documents monthly Credits per Team seat, reset at subscription-month end with no carryover. Individual plans also use subscription-month quota. Its `429 insufficient_quota` means plan exhaustion, while `429 Requests rate limit exceeded` is short request/token concurrency pressure with one-minute wait guidance. Owner-console Usage Analysis is a human surface, not a seat-key remaining endpoint. No such endpoint or quota header is documented there.

The separate [Coding Plan FAQ][qwen-coding] documents five-hour, weekly and monthly allocated-quota messages with calendar reset wording. Those signals do not apply to the Token Plan's `sk-sp-` seat key and hosts, kept separate in the [OLP adapter guide](../../internal/codeplans/qwen-token-plan/README.md). CLIProxyAPI has no Token Plan executor or body classifier.

An OLP classifier can identify monthly exhaustion, but an exact reset needs subscription-cycle metadata. Calendar month alone invents the anchor. Until a reset source exists, retain the retry fallback and let later pinned requests test recovery. Static seat-size weights remain possible without claiming live remaining quota.

### OpenCode Go

[Go][opencode-go] defines dollar limits per model, with five-hour, weekly and monthly windows at 20%, 50% and 100% of the monthly limit. Console usage is available, and Extra Usage can fall back to paid balance. The fetched Go and [Inference API][opencode-api] pages document no remaining-allowance endpoint or quota/reset header.

[Response cost fields][opencode-cost] distinguish `opencode.allowance`, the request's dollar charge against Go allowance, from `usage.cost`, `opencode.list_cost` and billed `opencode.tokens`. A charge proves neither remaining balance nor window anchor or reset. CLIProxyAPI has no allowance parser, leaving generic Retry-After behavior. Stable `x-opencode-session` supports routing/cache and is already [OLP conversation identity](../use/code-mode.md).

Observing charges across Chat, Messages and Responses could improve accounting or a clearly labeled least-OLP-observed-spend tie-break. It cannot establish vendor remaining allowance. Stronger estimates need plan and billing-anchor metadata plus a rule for work outside OLP. Local sums must stay out of admission, hard budgets and customer-facing remaining-quota claims.

## What OLP could adopt

OLP already has [weighted selection for new roots](../../internal/codesessions/store.go), per-family/model affinity, Codex exhaustion eligibility and response-status/Retry-After health for the other plans. [Code-mode contracts](../use/code-mode.md) remain authoritative. Useful adoption is adapter observation and classification, not a selector port.

[ADR 0005](../adr/0005-make-code-mode-better-than-magpie.md) accepts fresh proven bands for new roots, with existing weights inside the best band, exhausted accounts excluded and pins unchanged. Soonest reset supplies only a tie-break or refusal deadline. Unproven adapters keep weights plus cooldown. Active Codex and MiniMax reads need pinned schema, approved origins, isolated failures and fixture/live qualification before ordering.

The [current observation type](../../internal/codemode/observations.go) names `primary` and `secondary` with percentage pairs. A broader observation would need limit family, window, vendor unit, reported remaining or used value, reset when supplied, observation time and evidence label. GLM credits, Qwen Credits and Go charges cannot honestly become interchangeable percentages or OLP-computed vendor balances. The [plugin bucket contract][quota-plugin] offers a normalization idea, while OLP domain and storage work remain new.

Keep account-wide exhaustion, model entitlement, short concurrency and upstream capacity separate. Vendor scope and reset must justify each durable health state. [OLP attempt handling](../../internal/gateway/code_attempt.go) and CLIProxyAPI's cooldown split are reference points, not proof that one status means the same thing across adapters.

No signal alone widens recovery under [ADR 0002](../adr/0002-state-aware-code-account-recovery.md). Extending [`InitialRejection`](../../internal/codeadapter/recovery.go) requires an exact complete HTTP rejection proving non-execution, portable input, settled earlier attempts, published policy and controlled/live qualification. Streamed or ambiguous failures stay on the original account with usage uncertainty intact. The absence of client output is insufficient proof.

[selector]: https://github.com/router-for-me/CLIProxyAPI/blob/585f97deac9aa57c700301221fb1a7085e1827cc/sdk/cliproxy/auth/selector.go
[scheduler]: https://github.com/router-for-me/CLIProxyAPI/blob/585f97deac9aa57c700301221fb1a7085e1827cc/sdk/cliproxy/auth/scheduler.go
[routing-config]: https://github.com/router-for-me/CLIProxyAPI/blob/585f97deac9aa57c700301221fb1a7085e1827cc/config.example.yaml#L169-L242
[selection]: https://github.com/router-for-me/CLIProxyAPI/blob/585f97deac9aa57c700301221fb1a7085e1827cc/sdk/cliproxy/auth/conductor_selection.go#L995-L1628
[quota-state]: https://github.com/router-for-me/CLIProxyAPI/blob/585f97deac9aa57c700301221fb1a7085e1827cc/sdk/cliproxy/auth/types.go#L340
[quota-signals]: https://github.com/router-for-me/CLIProxyAPI/blob/585f97deac9aa57c700301221fb1a7085e1827cc/sdk/cliproxy/auth/quota_signals.go#L17
[codex-events]: https://github.com/router-for-me/CLIProxyAPI/blob/585f97deac9aa57c700301221fb1a7085e1827cc/internal/runtime/executor/helps/codex_quota.go#L29-L169
[devin-status]: https://github.com/router-for-me/CLIProxyAPI/blob/585f97deac9aa57c700301221fb1a7085e1827cc/internal/auth/devin/user_status.go#L335
[quota-plugin]: https://github.com/router-for-me/CLIProxyAPI/blob/585f97deac9aa57c700301221fb1a7085e1827cc/sdk/pluginapi/types.go#L1738-L1839
[quota-probe]: https://github.com/router-for-me/CLIProxyAPI/blob/585f97deac9aa57c700301221fb1a7085e1827cc/internal/api/handlers/management/plugin_quota.go#L61-L384
[auth-listing]: https://github.com/router-for-me/CLIProxyAPI/blob/585f97deac9aa57c700301221fb1a7085e1827cc/internal/api/handlers/management/auth_files.go#L874-L912
[cooldown]: https://github.com/router-for-me/CLIProxyAPI/blob/585f97deac9aa57c700301221fb1a7085e1827cc/sdk/cliproxy/auth/conductor_cooldown.go
[cooldown-constants]: https://github.com/router-for-me/CLIProxyAPI/blob/585f97deac9aa57c700301221fb1a7085e1827cc/sdk/cliproxy/auth/conductor_refresh.go#L37-L40
[cooldown-config]: https://github.com/router-for-me/CLIProxyAPI/blob/585f97deac9aa57c700301221fb1a7085e1827cc/config.example.yaml#L231-L242
[codex-errors]: https://github.com/router-for-me/CLIProxyAPI/blob/585f97deac9aa57c700301221fb1a7085e1827cc/internal/runtime/executor/codex_executor_terminal.go#L470-L531
[claude-resets]: https://github.com/router-for-me/CLIProxyAPI/blob/585f97deac9aa57c700301221fb1a7085e1827cc/internal/runtime/executor/helps/claude_ratelimit.go#L29-L231
[compat-retry]: https://github.com/router-for-me/CLIProxyAPI/blob/585f97deac9aa57c700301221fb1a7085e1827cc/internal/runtime/executor/openai_compat_executor.go#L1323-L1351
[execution]: https://github.com/router-for-me/CLIProxyAPI/blob/585f97deac9aa57c700301221fb1a7085e1827cc/sdk/cliproxy/auth/conductor_execution.go#L127-L517
[retry-config]: https://github.com/router-for-me/CLIProxyAPI/blob/585f97deac9aa57c700301221fb1a7085e1827cc/config.example.yaml#L204-L269
[stream-bootstrap]: https://github.com/router-for-me/CLIProxyAPI/blob/585f97deac9aa57c700301221fb1a7085e1827cc/sdk/cliproxy/auth/conductor_stream.go#L93
[codex-pricing]: https://learn.chatgpt.com/docs/pricing
[codex-usage]: https://github.com/openai/codex/blob/c0100ad4ecd2251c2b48b68534db08afac78c606/codex-rs/backend-client/src/client/rate_limit_resets.rs#L128-L133
[codex-limits]: https://github.com/openai/codex/blob/c0100ad4ecd2251c2b48b68534db08afac78c606/codex-rs/codex-api/src/rate_limits.rs#L54-L90
[codex-credits]: https://github.com/openai/codex/blob/c0100ad4ecd2251c2b48b68534db08afac78c606/codex-rs/codex-api/src/rate_limits.rs#L220-L232
[codex-limit-events]: https://github.com/openai/codex/blob/c0100ad4ecd2251c2b48b68534db08afac78c606/codex-rs/codex-api/src/rate_limits.rs#L104-L176
[codex-429]: https://github.com/openai/codex/blob/c0100ad4ecd2251c2b48b68534db08afac78c606/codex-rs/codex-api/src/api_bridge.rs#L208-L240
[openai-limits]: https://developers.openai.com/api/docs/guides/rate-limits
[glm-plan]: https://docs.z.ai/devpack/overview
[glm-faq]: https://docs.z.ai/devpack/faq
[glm-index]: https://docs.z.ai/llms.txt
[glm-errors]: https://docs.z.ai/api-reference/api-code
[mm-usage]: https://platform.minimax.io/docs/m-plan/usage-rules
[mm-faq]: https://platform.minimax.io/docs/m-plan/faq
[mm-cli]: https://platform.minimax.io/docs/m-plan/minimax-cli
[mm-errors]: https://platform.minimax.io/docs/api-reference/errorcode
[kimi-membership]: https://www.kimi.com/code/docs/en/kimi-code/membership.html
[kimi-errors]: https://www.kimi.com/code/docs/en/kimi-code/error-reference.html
[kimi-executor]: https://github.com/router-for-me/CLIProxyAPI/blob/585f97deac9aa57c700301221fb1a7085e1827cc/internal/runtime/executor/kimi_executor.go#L211-L218
[moonshot-balance]: https://platform.kimi.ai/docs/api/balance
[moonshot-limits]: https://platform.kimi.ai/docs/pricing/limits
[qwen-token]: https://www.alibabacloud.com/help/en/model-studio/token-plan-team-faq
[qwen-coding]: https://www.alibabacloud.com/help/en/model-studio/coding-plan-faq
[opencode-go]: https://opencode.ai/docs/go/
[opencode-api]: https://opencode.ai/v2/docs/console/api/inference/
[opencode-cost]: https://opencode.ai/v2/docs/console/api/inference/#cost
