# MiniMax and Kimi vendor client signals

Public-source snapshot, 2026-11-05 UTC. Pinned clients are [MiniMax CLI at 38aba5f](https://github.com/MiniMax-AI/cli/tree/38aba5f26173dd1065b115b221d79c9ddd87c6bb) and [Kimi Code at df90d90](https://github.com/MoonshotAI/kimi-code/tree/df90d90ec0714db23dcc830acd2db6232f5c7426). No credentials, account access or live inference or usage requests were used. Vendor code and fixtures establish client expectations, not authenticated live responses. User reports are identified separately below.

MiniMax exposes window fields and reconciles ambiguous counts against server percentages. Kimi classifies chat failures through structured SDK error fields, separately from its managed usage reader. Neither finding establishes OLP live qualification.

## MiniMax usage requests

[Region configuration][mm-regions] selects `https://api.minimax.io` for `global` and `https://api.minimax.cn` for `cn`, unless an explicit base URL overrides it. [Endpoint selection][mm-endpoints] uses `/account/query_balance`, without `/v1`, only for method `api-key` with an `sk-api-` token. All other credentials use `/v1/token_plan/remains`, including OAuth tokens starting with `sk-api-`. The [setup code][mm-setup] recognizes `sk-cp-` as a token plan key. The legacy raw-key helper assumes method `api-key`.

[Credential resolution][mm-credentials] prefers a flag API key, then refreshed stored OAuth, then a configured API key. Both the [SDK][mm-quota] and [`quota show`][mm-show] resolve credentials before choosing the endpoint. The [HTTP client][mm-http] defaults both usage calls to GET with bearer authorization and JSON content type. [SDK tests][mm-quota-tests] assert bearer auth without `x-api-key`, including the OAuth prefix case.

[Region detection][mm-detection] is a separate probe. It tries the selected usage endpoint with bearer auth and then `x-api-key` on each regional host, accepting only JSON with `base_resp.status_code == 0`.

## MiniMax response fields and interpretation

The [response types][mm-types] declare `QuotaResponse` as `model_remains` rows. The [fixture][mm-fixture] also carries `base_resp`, checked by the HTTP layer but absent from that type. Separate `AccountBalanceResponse` fields are string amounts `available_amount`, `cash_balance`, `voucher_balance`, `credit_balance`, `owed_amount` and `balance_alert_threshold`, plus boolean `balance_alert_switch` and required `base_resp`. The shared envelope has numeric `status_code` and string `status_msg`.

| Remains fields | Required | Client meaning |
| --- | --- | --- |
| `model_name` | Yes | String bucket name, otherwise passed through verbatim. |
| `start_time`, `end_time` | Yes | Interval bounds in epoch milliseconds. |
| `remains_time` | Yes | Interval time remaining in milliseconds, a duration. |
| `current_interval_total_count`, `current_interval_usage_count` | Yes | Numeric total and reported count, with ambiguous remaining-versus-consumed meaning. |
| `current_interval_remaining_percent`, `current_interval_status` | No | Server remaining percent and status. |
| `current_weekly_total_count`, `current_weekly_usage_count` | Yes | Weekly counterparts with the same count ambiguity. |
| `current_weekly_remaining_percent`, `current_weekly_status` | No | Weekly server remaining percent before boost, and status. |
| `weekly_start_time`, `weekly_end_time`, `weekly_remains_time` | Yes | Weekly epoch bounds and remaining duration, all in milliseconds. |
| `weekly_boost_permille` | No | Weekly display multiplier divided by 1,000. |

Every field except `model_name` is numeric. Type comments define statuses as 1 normal, 2 exhausted and 3 unlimited. [Date and duration consumers][mm-table] and the fixture establish millisecond units. The fixture text bucket spans five hours and its weekly bounds span seven days. Other buckets have different intervals, so those durations are not a universal schema rule.

### Counts and display

[`resolveQuotaCounts`][mm-counts] handles older remaining counts and newer consumed counts. With a finite server percentage it compares `count / total * 100` against `(total - count) / total * 100` and chooses the closer interpretation, rejecting it if the difference exceeds one percentage point. Nonfinite inputs, nonpositive totals or counts outside `[0, total]` return undefined. Without a server percentage it treats the count as remaining.

The [table renderer][mm-table] separately prefers the server percentage, falling back to raw count divided by total or zero for a nonpositive total. Weekly display applies `weekly_boost_permille / 1000`, defaulting to 1. Percentages are rounded and clamped to 0–200. Bars use the raw count, printed count pairs use reconciliation, and unresolved pairs display percentage text instead.

Rows retain API order and always have interval and weekly metrics. The interval countdown uses `remains_time`. Its label derives from `end_time - start_time`, formatting exact week or day multiples, then rounded hours or minutes. Weekly reset appears only as the first row's header date range. [Tests][mm-table-tests] include two-hour and daily intervals. Weekly status 3 displays unlimited, but both totals zero and both statuses 3 display not-in-plan. The interval has no equivalent unlimited branch. [`quota show`][mm-show] prints only interval rows in quiet mode and the unchanged raw response in JSON mode.

The renderer only renames `general` and `video` for Chinese display. Fixture and test names include `MiniMax-M*`, `MiniMax-M2`, `MiniMax-M3`, `speech-hd`, `image-01`, `general` and `video`. The fixture's speech and image rows have daily intervals and positive weekly totals, while the text row has zero weekly total. A test `general` row has zero totals, server percentages 94 and 98, and a two-hour interval. These examples suggest bucket roles but establish no server vocabulary or enforced model mapping. [Fixture][mm-fixture], [table tests][mm-table-tests] and [SDK tests][mm-quota-tests] are the evidence.

## MiniMax failures and inference evidence

Usage requests have no retry, endpoint fallback or partial rendering. The [HTTP layer][mm-http] maps non-2xx responses through the [shared error mapper][mm-errors], using an empty body when JSON parsing fails. Non-JSON 2xx responses become general errors. Nonzero `base_resp.status_code` in a JSON 2xx response goes through the mapper with synthetic HTTP status 200. SDK calls reject with `CLIError`. The [CLI handler][mm-handler] prints message and hint or JSON, then exits. Text-mode [`auth status`][mm-auth-status] alone softens usage failure to `Quota fetch failed` while still showing authentication state.

The [chat SDK][mm-text] and [command][mm-chat] POST Messages-shaped `model`, `messages` and `max_tokens` to `/anthropic/v1/messages` with `x-api-key`. There is no Chat Completions caller in this pinned client. Plain chat uses the same JSON checks as usage. Stream HTTP failures use the shared mapper before streaming starts, while malformed chunks after success warn and retain partial output.

| Mapper input | Precedence |
| --- | --- |
| Message | `base_resp.status_msg`, then `error.message`, then `HTTP {status}`. |
| Numeric code | `base_resp.status_code`, then `error.code`, with a truthy fallback so zero falls through. |
| Type and status | Type comes only from `error.type`. Status comes from fetch or synthetic 200. Declared `error.http_code` is unused. |

[Classification][mm-errors] checks HTTP status first. 401 and 403 mean auth, 429 means quota, 402 or `insufficient_balance_error` means quota, and 408 or 504 mean timeout. After endpoint-specific branches, codes 1002, 1039 and 1026 mean content filtering, 1028 and 1030 mean quota, and 2061 means model-not-in-plan quota. Code 1027 or output-sensitive wording also means content filtering. Other outcomes are general errors carrying the folded message and HTTP status. [Exit codes][mm-exits] are quota 4, auth 3, timeout 5 and content filtering 10.

Numeric codes embedded in message text are generally not parsed. The exception is a video-specific 422 `unprocessable_entity_error` branch accepting sensitive-content wording or a standalone 1026 token. Documented usage codes 2056, 2062 and 2067 have no mapper branch, so their result depends on HTTP status or becomes general error on a 200 business-error body.

### Inference replay remains unproven

For [#642](https://github.com/tyk-swe/olp/issues/642), the [vendor catalog][mm-catalog] identifies 2056 as usage limit, 2067 as usage limit with automatic credit consumption disabled, and 2062 as rate limit or no active subscription during usage reads. A catalog meaning alone proves neither an endpoint envelope nor non-execution.

The [Chat Completions schema][mm-openai-schema] declares `base_resp.status_code` and refers to the catalog without explicitly showing 2056 or 2067. The [Messages schema][mm-messages-schema] declares `error.type` and `error.message`, omitting numeric `error.code`. That omission does not prove an additional field can never appear. The CLI reads both numeric envelopes, but that is client behavior.

[MiniMax Code's classifier][mm-code-classifier] and [retry test][mm-code-test], pinned at `3c21163c85a9684030cb147bfef48c6284fa3c46`, use upstream or synthetic top-level status codes. A [user report][mm-inference-report] puts 2056 in `error.message`, without numeric `error.code`. No first-party source found establishes either numeric inference envelope for 2056 or 2067, or guarantees non-execution. Required replay evidence remains unmet.

### Regional remains fixtures remain unproven

For [#647](https://github.com/tyk-swe/olp/issues/647), the [global][mm-global-faq] and [mainland][mm-cn-faq] M Plan FAQs document bearer GET examples on `https://www.minimax.io/v1/token_plan/remains` and `https://www.minimax.cn/v1/token_plan/remains`. These differ from the CLI's `api` hosts. The difference establishes neither failure nor interchangeability and authorizes no host or profile qualification change.

The vendor fixture and types establish expected schema without authenticated provenance or region. SDK tests use a local mock. [User report #53][mm-region-report] supplies regional example JSON without identifying an `sk-cp-` key. [Report #165][mm-partial-report] supplies a partial row and discusses both regions, without complete known-key fixtures for either.

Neither global nor mainland meets the real-key fixture requirement. Explicit exclusion of an unpinned profile is a separate option in #647. This investigation does not qualify remains observations for ordering.

## Kimi request protocols

Provider type `kimi` covers both the paid default `https://api.moonshot.ai/v1` and managed subscription endpoints `https://api.kimi.com/coding/v1` and `https://api.kimi.ai/coding/v1`. [`KIMI_CODE_BASE_URL`][kimi-usage] overrides the managed default. [Login provisioning][kimi-auth] writes a managed provider, and [connection resolution][kimi-connection] prefers configured model or provider URLs over the [protocol default][kimi-protocol].

[Managed model metadata][kimi-models] maps `anthropic` to Anthropic and `response` to Responses. Anthropic models use beta Messages at `/v1/messages?beta=true`. Undeclared protocols fall back to OpenAI through [model resolution][kimi-model-auth]. The [provider definitions][kimi-provider] register all three protocols with the same quota classifier.

Official [OpenAI][kimi-openai] and [Anthropic][kimi-anthropic] SDKs perform requests with `maxRetries: 0`. OpenAI chat supports plain and streamed calls, with `include_usage` on streams. Anthropic selects regular or beta Messages. [Responses][kimi-responses] reuses OpenAI error decoding. The [registry][kimi-registry] defaults to streaming. [Endpoint resolution][kimi-endpoint] passes credentials as an API-key string, including OAuth access tokens, leaving concrete auth headers to the SDK.

[Kimi overrides][kimi-protocol] rename the OpenAI output cap to `max_completion_tokens`, convert `$`-prefixed tools to built-ins, emit a `thinking` object, hoist per-choice usage and read `x-trace-id`. Anthropic uses the same trace header and error wrapper.

## Kimi chat errors and retries

Chat failures enter through SDK API-error objects, never a local raw-body JSON parser. The [normalized error type][kimi-error-type] carries message, status, request ID, Retry-After milliseconds, headers and retryability. Kimi's [wrapper][kimi-protocol] and registry try the [provider quota classifier][kimi-quota-errors] before base decoding outside streams. Stream errors go directly to their base decoder.

The provider classifier follows nested `error` objects up to three levels, collecting string `code` and `type` values. Only `exceeded_current_quota_error` with top-level status 429 or absent yields nonretryable `quota_exhausted`. It reads either SDK message nesting, top-level `requestID` and `headers`. [Tests][kimi-error-tests] reject message-only insufficient-balance wording and quota codes attached to non-429 statuses.

| Decoder behavior | OpenAI and Responses | Anthropic |
| --- | --- | --- |
| SDK shape | Reads the extracted `error` object. Code and type prefer SDK-lifted fields, then body fields. | Reads the SDK body with inner `error`. Message and type come from that inner object, with SDK type fallback. No code field is read. |
| Quota | `insufficient_quota` or `exceeded_current_quota_error` in lifted or body code/type, at status 429 or absent, yields nonretryable exhaustion. | Checks the same quota strings against type. |
| Other structured errors | Overload code/type yields retryable overload at any status. `context_length_exceeded` code is nonretryable overflow. Rate codes are consulted only without HTTP status. | Checks quota, overload and rate strings against type. |

Sources are the pinned [OpenAI decoder][kimi-openai-errors] and [Anthropic decoder][kimi-anthropic-errors]. Rate strings are `rate_limit_exceeded`, `rate_limit_reached_error` and `rate_limit_error`. Overload strings are `overloaded_error` and `engine_overloaded_error`.

Shared [status rules][kimi-status] classify unmatched 429 as retryable rate limit. They detect context overflow at 400, 413 and 422, request size at 413, overload at 529 or qualifying 500/503 messages, and request-structure or image-format errors. Other statuses remain generic, retryable for 408, 409, 429, 500, 502, 503, 504 and 529. Stream decoders discard HTTP status and map business codes 429, 529 and 413 to rate limit, overload and request-too-large respectively. [Retry-After parsing][kimi-http] accepts delta seconds only and converts to milliseconds.

The pinned chat path has no classifier for documented five-hour, weekly, monthly or concurrency message wording. Generic overload and higher-level [rate-limit wording rules][kimi-contract-errors] do not supply that distinction. A quota message stops retries only with a structured quota code. Plain 429 concurrency, quota or overload messages follow generic rate-limit handling unless another structured branch applies.

The [local retry policy][kimi-retry] allows retryable errors other than syntax or abort errors, defaults to ten attempts, and doubles jittered delays from 500 milliseconds up to 32 seconds unless Retry-After overrides them. SDK retries remain disabled.

## Kimi managed usage

The separate [managed usage reader][kimi-usage] calls `GET {base}/usages` with bearer auth and parses `limit_5h`, `limit_7d`, `limit_month_total` and `limit_month_code`. Each entry has numeric or numeric-string `used_ratio` and optional string `reset_time`. This vendor-client contract does not establish subscription API-key acceptance.

Usage failures use the OAuth package's raw-body [message extractor][kimi-usage-errors], which tries top-level `error_description`, `message`, `detail`, then string `error`, then nested `error` fields `message`, `error_description`, `detail`, `code` and `type`. Keep this reader distinct from SDK chat-error decoding when adopting either behavior.

[mm-regions]: https://github.com/MiniMax-AI/cli/blob/38aba5f26173dd1065b115b221d79c9ddd87c6bb/src/config/schema.ts#L1-L4
[mm-endpoints]: https://github.com/MiniMax-AI/cli/blob/38aba5f26173dd1065b115b221d79c9ddd87c6bb/src/client/endpoints.ts#L49-L92
[mm-setup]: https://github.com/MiniMax-AI/cli/blob/38aba5f26173dd1065b115b221d79c9ddd87c6bb/src/commands/agent/setup.ts#L118-L135
[mm-credentials]: https://github.com/MiniMax-AI/cli/blob/38aba5f26173dd1065b115b221d79c9ddd87c6bb/src/auth/resolver.ts#L9-L32
[mm-quota]: https://github.com/MiniMax-AI/cli/blob/38aba5f26173dd1065b115b221d79c9ddd87c6bb/src/sdk/quota/index.ts#L18-L29
[mm-show]: https://github.com/MiniMax-AI/cli/blob/38aba5f26173dd1065b115b221d79c9ddd87c6bb/src/commands/quota/show.ts#L20-L68
[mm-http]: https://github.com/MiniMax-AI/cli/blob/38aba5f26173dd1065b115b221d79c9ddd87c6bb/src/client/http.ts#L99-L193
[mm-quota-tests]: https://github.com/MiniMax-AI/cli/blob/38aba5f26173dd1065b115b221d79c9ddd87c6bb/test/sdk/quota.test.ts#L12-L202
[mm-detection]: https://github.com/MiniMax-AI/cli/blob/38aba5f26173dd1065b115b221d79c9ddd87c6bb/src/config/detect-region.ts#L9-L58
[mm-types]: https://github.com/MiniMax-AI/cli/blob/38aba5f26173dd1065b115b221d79c9ddd87c6bb/src/types/api.ts#L337-L421
[mm-fixture]: https://github.com/MiniMax-AI/cli/blob/38aba5f26173dd1065b115b221d79c9ddd87c6bb/test/fixtures/quota-response.json
[mm-table]: https://github.com/MiniMax-AI/cli/blob/38aba5f26173dd1065b115b221d79c9ddd87c6bb/src/output/quota-table.ts#L40-L262
[mm-counts]: https://github.com/MiniMax-AI/cli/blob/38aba5f26173dd1065b115b221d79c9ddd87c6bb/src/utils/quota.ts#L9-L51
[mm-table-tests]: https://github.com/MiniMax-AI/cli/blob/38aba5f26173dd1065b115b221d79c9ddd87c6bb/test/output/quota-table.test.ts#L40-L223
[mm-errors]: https://github.com/MiniMax-AI/cli/blob/38aba5f26173dd1065b115b221d79c9ddd87c6bb/src/errors/api.ts#L4-L129
[mm-handler]: https://github.com/MiniMax-AI/cli/blob/38aba5f26173dd1065b115b221d79c9ddd87c6bb/src/errors/handler.ts#L5-L19
[mm-auth-status]: https://github.com/MiniMax-AI/cli/blob/38aba5f26173dd1065b115b221d79c9ddd87c6bb/src/commands/auth/status.ts#L63-L71
[mm-text]: https://github.com/MiniMax-AI/cli/blob/38aba5f26173dd1065b115b221d79c9ddd87c6bb/src/sdk/text/index.ts#L34-L48
[mm-chat]: https://github.com/MiniMax-AI/cli/blob/38aba5f26173dd1065b115b221d79c9ddd87c6bb/src/commands/text/chat.ts#L244-L301
[mm-exits]: https://github.com/MiniMax-AI/cli/blob/38aba5f26173dd1065b115b221d79c9ddd87c6bb/src/errors/codes.ts#L1-L9
[mm-catalog]: https://platform.minimax.io/docs/api-reference/errorcode
[mm-openai-schema]: https://platform.minimax.io/docs/api-reference/text/api/openapi-chat-openai.json
[mm-messages-schema]: https://platform.minimax.io/docs/api-reference/text/api/openapi-chat-anthropic.json
[mm-code-classifier]: https://github.com/MiniMax-AI/minimax-code/blob/3c21163c85a9684030cb147bfef48c6284fa3c46/packages/shared/src/llm-error-classifier.ts#L858-L869
[mm-code-test]: https://github.com/MiniMax-AI/minimax-code/blob/3c21163c85a9684030cb147bfef48c6284fa3c46/packages/agent-core/test/unit/pi-turn-runner/llm-retry.test.ts#L362-L382
[mm-inference-report]: https://github.com/MiniMax-AI/MiniMax-M2.7/issues/36
[mm-global-faq]: https://www.minimax.io/m-plan
[mm-cn-faq]: https://www.minimax.cn/m-plan
[mm-region-report]: https://github.com/MiniMax-AI/cli/issues/53
[mm-partial-report]: https://github.com/MiniMax-AI/cli/issues/165
[kimi-usage]: https://github.com/MoonshotAI/kimi-code/blob/df90d90ec0714db23dcc830acd2db6232f5c7426/packages/oauth/src/managed-usage.ts#L8-L256
[kimi-auth]: https://github.com/MoonshotAI/kimi-code/blob/df90d90ec0714db23dcc830acd2db6232f5c7426/packages/agent-core-v2/src/app/auth/authService.ts#L563-L576
[kimi-connection]: https://github.com/MoonshotAI/kimi-code/blob/df90d90ec0714db23dcc830acd2db6232f5c7426/packages/agent-core-v2/src/human/llm-requester/connection.ts#L23-L43
[kimi-protocol]: https://github.com/MoonshotAI/kimi-code/blob/df90d90ec0714db23dcc830acd2db6232f5c7426/packages/agent-core-v2/src/human/llm/builtin/provider/kimi/protocol.ts#L31-L207
[kimi-models]: https://github.com/MoonshotAI/kimi-code/blob/df90d90ec0714db23dcc830acd2db6232f5c7426/packages/oauth/src/managed-kimi-code.ts#L16-L626
[kimi-model-auth]: https://github.com/MoonshotAI/kimi-code/blob/df90d90ec0714db23dcc830acd2db6232f5c7426/packages/agent-core-v2/src/llm-adapter/model/model-auth.ts#L140-L161
[kimi-provider]: https://github.com/MoonshotAI/kimi-code/blob/df90d90ec0714db23dcc830acd2db6232f5c7426/packages/agent-core-v2/src/llm-adapter/provider/provider-definition.ts#L196-L228
[kimi-openai]: https://github.com/MoonshotAI/kimi-code/blob/df90d90ec0714db23dcc830acd2db6232f5c7426/packages/agent-core-v2/src/human/llm/builtin/protocol/openai/index.ts#L45-L82
[kimi-anthropic]: https://github.com/MoonshotAI/kimi-code/blob/df90d90ec0714db23dcc830acd2db6232f5c7426/packages/agent-core-v2/src/human/llm/builtin/protocol/anthropic/index.ts#L67-L120
[kimi-responses]: https://github.com/MoonshotAI/kimi-code/blob/df90d90ec0714db23dcc830acd2db6232f5c7426/packages/agent-core-v2/src/human/llm/builtin/protocol/openai-responses/index.ts#L1-L15
[kimi-registry]: https://github.com/MoonshotAI/kimi-code/blob/df90d90ec0714db23dcc830acd2db6232f5c7426/packages/agent-core-v2/src/llm-adapter/protocol/protocolAdapterRegistry.ts#L239-L250
[kimi-endpoint]: https://github.com/MoonshotAI/kimi-code/blob/df90d90ec0714db23dcc830acd2db6232f5c7426/packages/agent-core-v2/src/human/llm/endpoint.ts#L29-L37
[kimi-error-type]: https://github.com/MoonshotAI/kimi-code/blob/df90d90ec0714db23dcc830acd2db6232f5c7426/packages/agent-core-v2/src/human/llm/response.ts#L27-L83
[kimi-quota-errors]: https://github.com/MoonshotAI/kimi-code/blob/df90d90ec0714db23dcc830acd2db6232f5c7426/packages/agent-core-v2/src/human/llm/builtin/provider/kimi/errors.ts#L4-L56
[kimi-error-tests]: https://github.com/MoonshotAI/kimi-code/blob/df90d90ec0714db23dcc830acd2db6232f5c7426/packages/agent-core-v2/src/human/test/llm/errors.test.ts#L361-L421
[kimi-openai-errors]: https://github.com/MoonshotAI/kimi-code/blob/df90d90ec0714db23dcc830acd2db6232f5c7426/packages/agent-core-v2/src/human/llm/builtin/protocol/openai/decode.ts#L359-L460
[kimi-anthropic-errors]: https://github.com/MoonshotAI/kimi-code/blob/df90d90ec0714db23dcc830acd2db6232f5c7426/packages/agent-core-v2/src/human/llm/builtin/protocol/anthropic/decode.ts#L230-L295
[kimi-status]: https://github.com/MoonshotAI/kimi-code/blob/df90d90ec0714db23dcc830acd2db6232f5c7426/packages/agent-core-v2/src/human/llm/protocol/rules/status.ts#L27-L196
[kimi-http]: https://github.com/MoonshotAI/kimi-code/blob/df90d90ec0714db23dcc830acd2db6232f5c7426/packages/agent-core-v2/src/human/llm/protocol/kit/http.ts#L26-L37
[kimi-contract-errors]: https://github.com/MoonshotAI/kimi-code/blob/df90d90ec0714db23dcc830acd2db6232f5c7426/packages/agent-core-v2/src/llm-adapter/contract/errors.ts#L343-L353
[kimi-retry]: https://github.com/MoonshotAI/kimi-code/blob/df90d90ec0714db23dcc830acd2db6232f5c7426/packages/agent-core-v2/src/human/llm/policy/retry.ts#L3-L55
[kimi-usage-errors]: https://github.com/MoonshotAI/kimi-code/blob/df90d90ec0714db23dcc830acd2db6232f5c7426/packages/oauth/src/api-error.ts#L6-L42
