<script lang="ts">
  import { errorMessage } from '$lib/api/http';

  import {
    formatCost,
    formatDate,
    formatInteger,
    statusLabel,
    statusTone
  } from '$lib/format';

  import type { CreateQueryResult } from '@tanstack/svelte-query';
  import type { RequestDetail } from '$lib/api/requests';
  let { detail }: { detail: CreateQueryResult<RequestDetail> } = $props();
</script>

{#if detail.isPending}
  <div class="loading-state" role="status">Loading request timeline…</div>
{:else if detail.isError}
  <div class="inline-problem" role="alert">
    {errorMessage(detail.error, 'The request timeline could not be loaded.')}
    <button class="text-button" onclick={() => detail.refetch()}
      >Try again</button
    >
  </div>
{:else if detail.data}
  <section class="metric-grid" aria-label="Request summary">
    <article class="card metric-card">
      <p>Status</p>
      <strong
        ><span
          class="badge {statusTone(
            detail.data.status_code,
            detail.data.error_class
          )}"
          >{statusLabel(detail.data.status_code, detail.data.error_class)}</span
        ></strong
      >
    </article>
    <article class="card metric-card">
      <p>Total latency</p>
      <strong
        >{detail.data.total_latency_ms == null
          ? '—'
          : `${detail.data.total_latency_ms} ms`}</strong
      >
    </article>
    <article class="card metric-card">
      <p>First byte (TTFT)</p>
      <strong
        >{detail.data.first_byte_ms == null
          ? '—'
          : `${detail.data.first_byte_ms} ms`}</strong
      >
    </article>
    <article class="card metric-card">
      <p>Estimated cost</p>
      <strong class:unpriced={detail.data.unpriced}
        >{formatCost(detail.data.estimated_cost, detail.data.currency)}</strong
      >
    </article>
  </section>

  <section class="card request-facts" aria-labelledby="decision-title">
    <div>
      <p class="eyebrow">Route decision</p>
      <h2 id="decision-title">{detail.data.route}</h2>
    </div>
    <dl>
      <div>
        <dt>Operation</dt>
        <dd>{detail.data.operation}</dd>
      </div>
      <div>
        <dt>Client surface</dt>
        <dd>{detail.data.surface}</dd>
      </div>
      <div>
        <dt>Runtime generation</dt>
        <dd class="mono">{detail.data.runtime_generation_id}</dd>
      </div>
      <div>
        <dt>API key ID</dt>
        <dd class="mono">{detail.data.api_key_id}</dd>
      </div>
      <div>
        <dt>Input tokens</dt>
        <dd>{formatInteger(detail.data.input_tokens)}</dd>
      </div>
      <div>
        <dt>Cached input tokens</dt>
        <dd>{formatInteger(detail.data.cached_input_tokens)}</dd>
      </div>
      <div>
        <dt>Output tokens</dt>
        <dd>{formatInteger(detail.data.output_tokens)}</dd>
      </div>
      <div>
        <dt>Usage completeness</dt>
        <dd>
          <span
            class="badge"
            class:success={detail.data.usage_complete === true}
            class:warning={detail.data.usage_complete === false}
            >{detail.data.usage_complete == null
              ? 'Unknown'
              : detail.data.usage_complete
                ? 'Complete'
                : 'Incomplete'}</span
          >
        </dd>
      </div>
      <div>
        <dt>Started</dt>
        <dd>{formatDate(detail.data.started_at)}</dd>
      </div>
      <div>
        <dt>Completed</dt>
        <dd>
          {detail.data.completed_at
            ? formatDate(detail.data.completed_at)
            : 'Still in flight'}
        </dd>
      </div>
    </dl>
  </section>

  <section class="timeline-section" aria-labelledby="attempts-title">
    <div class="section-heading">
      <div>
        <p class="eyebrow">Upstream</p>
        <h2 id="attempts-title">Attempt timeline</h2>
      </div>
      <span class="badge">{detail.data.attempts.length} attempts</span>
    </div>
    {#if detail.data.attempts.length === 0}
      <div class="card empty-state">No attempt metadata was recorded.</div>
    {:else}
      <ol class="timeline">
        {#each detail.data.attempts as attempt (attempt.id)}
          <li class="card">
            <span class="timeline-marker" aria-hidden="true"
              >{attempt.ordinal}</span
            >
            <div class="attempt-heading">
              <div>
                <strong>{attempt.provider_name}</strong><span class="mono"
                  >{attempt.upstream_model}</span
                >
              </div>
              <span
                class="badge {statusTone(
                  attempt.status_code,
                  attempt.error_class
                )}"
                >{statusLabel(attempt.status_code, attempt.error_class)}</span
              >
            </div>
            <dl>
              <div>
                <dt>Started</dt>
                <dd>{formatDate(attempt.started_at)}</dd>
              </div>
              <div>
                <dt>Completed</dt>
                <dd>
                  {attempt.completed_at
                    ? formatDate(attempt.completed_at)
                    : 'Still in flight'}
                </dd>
              </div>
              <div>
                <dt>First byte</dt>
                <dd>
                  {attempt.first_byte_ms === null ||
                  attempt.first_byte_ms === undefined
                    ? '—'
                    : `${attempt.first_byte_ms} ms`}
                </dd>
              </div>
              <div>
                <dt>Latency</dt>
                <dd>
                  {attempt.latency_ms === null ||
                  attempt.latency_ms === undefined
                    ? '—'
                    : `${attempt.latency_ms} ms`}
                </dd>
              </div>
              <div>
                <dt>Response committed</dt>
                <dd>{attempt.committed ? 'Yes — failover stopped' : 'No'}</dd>
              </div>
            </dl>
          </li>
        {/each}
      </ol>
    {/if}
  </section>
{/if}

<style>
  .filters {
    margin-top: 1.5rem;
    padding: 1rem;
  }
  .filter-grid {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 0.8rem;
  }

  .filter-actions {
    display: flex;
    gap: 0.65rem;
    margin-top: 1rem;
  }
  .result-note {
    margin: 0;
    color: var(--foreground-muted);
  }
  .text-button {
    padding: 0.4rem 0.65rem;
  }
  .text-button:hover {
    text-decoration: underline;
  }

  .warning-text,
  .unpriced {
    color: var(--warning);
  }
  .row-link {
    display: inline-flex;
    min-height: 2.75rem;
    align-items: center;
    color: var(--accent-strong);
    font-weight: 700;
  }
  .mobile-results {
    display: none;
    margin: 0;
    padding: 0;
    list-style: none;
  }

  .mobile-result-heading {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 0.75rem;
  }

  .pagination {
    display: flex;
    align-items: center;
    justify-content: flex-end;
    gap: 1rem;
    margin-top: 1rem;
  }

  .request-facts {
    margin-top: 1rem;
    padding: 1.25rem;
  }
  h2 {
    margin: 0;
    font-size: 1.2rem;
    letter-spacing: -0.025em;
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
    color: var(--foreground-muted);
    font-size: 0.72rem;
    font-weight: 700;
  }
  dd {
    overflow-wrap: anywhere;
    margin: 0.2rem 0 0;
  }
  .timeline-section {
    margin-top: 2rem;
  }
  .section-heading,
  .attempt-heading {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 1rem;
  }
  .timeline {
    display: grid;
    gap: 0.75rem;
    margin: 1rem 0 0;
    padding: 0;
    list-style: none;
  }
  .timeline li {
    position: relative;
    margin-left: 1.4rem;
    padding: 1rem 1rem 1rem 1.5rem;
  }
  .timeline-marker {
    position: absolute;
    top: 0.85rem;
    left: -1.4rem;
    display: grid;
    width: 2rem;
    height: 2rem;
    place-items: center;
    border-radius: 999px;
    background: var(--accent);
    color: white;
    font-size: 0.75rem;
    font-weight: 800;
  }
  .attempt-heading strong,
  .attempt-heading .mono {
    display: block;
  }
  .attempt-heading .mono {
    margin-top: 0.15rem;
    color: var(--foreground-muted);
    font-size: 0.78rem;
  }
  .timeline dl {
    grid-template-columns: repeat(auto-fit, minmax(9rem, 1fr));
  }
  @media (max-width: 72rem) {
    .filter-grid {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
    dl,
    .timeline dl {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
  }
  @media (max-width: 44rem) {
    .desktop-results {
      display: none;
    }
    .mobile-results {
      display: grid;
      gap: 0.75rem;
    }
  }
  @media (max-width: 40rem) {
    .filter-grid,
    dl,
    .timeline dl {
      grid-template-columns: 1fr;
    }

    .filters {
      padding: 0.85rem;
    }
    .pagination {
      justify-content: space-between;
    }
  }
  @media (forced-colors: active) {
    .timeline-marker {
      border: 1px solid CanvasText;
    }
  }
</style>
