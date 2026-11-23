<script lang="ts">
  import { resolve } from '$app/paths';
  import {
    formatCost,
    formatDate,
    formatInteger,
    statusLabel,
    statusTone
  } from '#lib/format.js';
  import type { RequestDetail } from '#lib/features/usage/history/api.js';

  let { data }: { data: RequestDetail } = $props();
</script>

<section class="metric-grid" aria-label="Request summary">
  <article class="card metric-card">
    <p>Status</p>
    <strong
      ><span class="badge {statusTone(data.status_code, data.error_class)}"
        >{statusLabel(data.status_code, data.error_class)}</span
      ></strong
    >
  </article>
  <article class="card metric-card">
    <p>Total latency</p>
    <strong
      >{data.total_latency_ms == null
        ? '—'
        : `${data.total_latency_ms} ms`}</strong
    >
  </article>
  <article class="card metric-card">
    <p>First byte</p>
    <strong
      >{data.first_byte_ms == null ? '—' : `${data.first_byte_ms} ms`}</strong
    >
  </article>
  <article class="card metric-card">
    <p>Estimated cost</p>
    <strong class:unpriced={data.unpriced}
      >{formatCost(data.estimated_cost, data.currency)}</strong
    >
    {#if data.charge != null}<small
        >Charge {formatCost(data.charge, data.currency)}</small
      >{/if}
    {#if data.unpriced}<small class="unpriced"
        >Unpriced — no published price covered part of this request.</small
      >{/if}
    {#if data.estimated_cost != null}<details>
        <summary>Exact amount</summary>
        <p>{data.estimated_cost} {data.currency ?? ''}</p>
      </details>{/if}
  </article>
  {#if data.attribution?.session}
    <article class="card metric-card">
      <p>Session</p>

      <strong>
        <a
          href={resolve(
            `requests?session_id=${encodeURIComponent(data.attribution.session)}`
          )}
          class="mono">{data.attribution.session}</a
        >
      </strong>

      <span>Requests in this session</span>
    </article>
  {/if}
</section>

<section class="card request-facts" aria-labelledby="decision-title">
  <div>
    <p class="eyebrow">Route decision</p>
    <h2 id="decision-title">{data.route}</h2>
  </div>
  <dl>
    <div>
      <dt>Operation</dt>
      <dd>{data.operation}</dd>
    </div>
    <div>
      <dt>Client surface</dt>
      <dd>{data.surface}</dd>
    </div>
    <div>
      <dt>Runtime generation</dt>
      <dd class="mono">{data.runtime_generation_id}</dd>
    </div>
    <div>
      <dt>API key ID</dt>
      <dd class="mono">{data.api_key_id}</dd>
    </div>
    <div>
      <dt>Input tokens</dt>
      <dd>{formatInteger(data.input_tokens)}</dd>
    </div>
    <div>
      <dt>Cached input tokens</dt>
      <dd>{formatInteger(data.cached_input_tokens)}</dd>
    </div>
    <div>
      <dt>Cache write tokens</dt>
      <dd>
        {formatInteger(data.cache_write_input_tokens)}
        {#if data.cache_write_5m_input_tokens || data.cache_write_1h_input_tokens}
          <small
            >5m {formatInteger(data.cache_write_5m_input_tokens)} · 1h {formatInteger(
              data.cache_write_1h_input_tokens
            )}</small
          >
        {/if}
      </dd>
    </div>
    <div>
      <dt>Output tokens</dt>
      <dd>{formatInteger(data.output_tokens)}</dd>
    </div>
    <div>
      <dt>Capture</dt>
      <dd>
        {#if data.payload_captured}<span
            class="badge"
            title="Queued in the volatile capture queue — does not guarantee delivery"
            >Capture queued</span
          ><small>Queued ≠ delivered — see audit records.</small>{:else}Not
          captured{/if}
      </dd>
    </div>
    <div>
      <dt>Usage completeness</dt>
      <dd>
        <span
          class="badge"
          class:success={data.usage_complete === true}
          class:warning={data.usage_complete === false}
        >
          {data.usage_complete == null
            ? 'Unknown'
            : data.usage_complete
              ? 'Complete'
              : 'Incomplete'}
        </span>
      </dd>
    </div>
    {#if data.attribution?.session}
      <div>
        <dt>Session</dt>
        <dd>
          <a
            href={resolve(
              `requests?session_id=${encodeURIComponent(data.attribution.session)}`
            )}
            class="mono">{data.attribution.session}</a
          >
        </dd>
      </div>
    {/if}
    <div>
      <dt>Started</dt>
      <dd>{formatDate(data.started_at)}</dd>
    </div>
    <div>
      <dt>Completed</dt>
      <dd>
        {data.completed_at ? formatDate(data.completed_at) : 'Still in flight'}
      </dd>
    </div>
  </dl>
  {#if data.guardrail_decisions?.length}
    <div class="policy-decisions">
      <p class="eyebrow">Guardrail decisions</p>
      <ul>
        {#each data.guardrail_decisions as decision (`${decision.guardrail_id}-${decision.revision}-${decision.phase}-${decision.outcome}`)}
          <li>
            <code>{decision.guardrail_id}</code> · {decision.phase} · {decision.action}
            · {decision.outcome} · {decision.latency_ms} ms
            {#if decision.categories?.length}<small
                >{decision.categories.join(', ')}</small
              >{/if}
          </li>
        {/each}
      </ul>
    </div>
  {/if}
</section>

<style>
  .unpriced {
    color: var(--warning);
  }
  small.unpriced {
    display: block;
    margin-top: 0.3rem;
    font-size: var(--text-caption);
  }
  .request-facts {
    margin-top: 1rem;
    padding: 1.5rem;
  }
  .policy-decisions {
    margin-top: 1.25rem;
    padding-top: 1rem;
    border-top: 1px solid var(--border-hairline);
  }
  .policy-decisions ul {
    margin: 0.4rem 0;
    padding-left: 1rem;
  }
  .policy-decisions li {
    overflow-wrap: anywhere;
  }
  .policy-decisions small {
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  h2 {
    margin: 0;
    font-size: var(--text-body);
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  dl {
    display: grid;
    grid-template-columns: repeat(4, minmax(0, 1fr));
    gap: 1rem;
    margin: 1.25rem 0 0;
  }
  dl div {
    min-width: 0;
  }
  dt {
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-weight: 400;
    letter-spacing: -0.02em;
    text-transform: uppercase;
  }
  dd {
    overflow-wrap: anywhere;
    margin: 0.25rem 0 0;
  }
  .provider-request-id {
    grid-column: 1 / -1;
  }
  @media (max-width: 72rem) {
    dl {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
  }
  @media (max-width: 40rem) {
    dl {
      grid-template-columns: 1fr;
    }
  }
</style>
