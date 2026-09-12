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
  import type {
    RequestAttempt,
    RequestDetail
  } from '$lib/features/usage/history/api';
  let { detail }: { detail: CreateQueryResult<RequestDetail> } = $props();

  function chargeLabel(status: RequestAttempt['charge_status']) {
    switch (status) {
      case 'billable':
        return 'Billable';
      case 'not_billable':
        return 'Not billable';
      case 'billing_uncertain':
        return 'Billing uncertain';
      default:
        return 'Unknown';
    }
  }

  function attemptCost(attempt: RequestAttempt) {
    if (attempt.estimated_cost != null) {
      return attempt.estimated_cost;
    }
    if (attempt.charge_status === 'not_billable') return 'Not applicable';
    if (attempt.unpriced === true) return 'Unpriced';
    if (attempt.usage_complete === false)
      return 'Unavailable (incomplete usage)';
    return 'Unknown';
  }
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
      <p>First byte</p>
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
      {#if detail.data.estimated_cost != null}<details>
          <summary>Exact amount</summary>
          <p>{detail.data.estimated_cost} {detail.data.currency ?? ''}</p>
        </details>{/if}
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
            {#if attempt.routing}
              <dl aria-label="Credential and routing provenance">
                {#if attempt.routing.policy}<div>
                    <dt>Routing policy</dt>
                    <dd class="mono">
                      {attempt.routing.policy.strategy} · {attempt.routing
                        .policy.digest}
                    </dd>
                  </div>{/if}
                <div>
                  <dt>Credential slot</dt>
                  <dd class="mono">
                    {attempt.routing.credential_slot_id ??
                      'Connection authentication'}
                  </dd>
                </div>
                <div>
                  <dt>Credential version</dt>
                  <dd class="mono">
                    {attempt.routing.credential_version_id ?? 'None'}
                  </dd>
                </div>
                <div>
                  <dt>Provider revision</dt>
                  <dd class="mono">{attempt.routing.provider_revision_id}</dd>
                </div>
                <div>
                  <dt>Selected price revision</dt>
                  <dd class="mono">
                    {attempt.routing.pricing_revision_id ??
                      'Unpriced at dispatch'}
                  </dd>
                </div>
                <div>
                  <dt>First meaningful output</dt>
                  <dd>
                    {attempt.routing.first_output_ms == null
                      ? 'Unknown'
                      : `${attempt.routing.first_output_ms} ms`}
                  </dd>
                </div>
              </dl>
            {/if}
            {#if attempt.charge_status == null}
              <p>No usage or pricing facts were recorded.</p>
            {:else}
              <dl aria-label="Attempt usage and pricing">
                <div>
                  <dt>Charge status</dt>
                  <dd>{chargeLabel(attempt.charge_status)}</dd>
                </div>
                <div>
                  <dt>Usage observed</dt>
                  <dd>
                    {attempt.usage_observed == null
                      ? 'Unknown'
                      : attempt.usage_observed
                        ? 'Yes'
                        : 'No'}
                  </dd>
                </div>
                <div>
                  <dt>Usage completeness</dt>
                  <dd>
                    {attempt.usage_complete == null
                      ? 'Unknown'
                      : attempt.usage_complete
                        ? 'Complete'
                        : 'Incomplete'}
                  </dd>
                </div>
                <div>
                  <dt>Input tokens</dt>
                  <dd>{formatInteger(attempt.input_tokens)}</dd>
                </div>
                <div>
                  <dt>Cached input tokens</dt>
                  <dd>{formatInteger(attempt.cached_input_tokens)}</dd>
                </div>
                <div>
                  <dt>Output tokens</dt>
                  <dd>{formatInteger(attempt.output_tokens)}</dd>
                </div>
                <div>
                  <dt>Media units</dt>
                  <dd>{attempt.media_units ?? '—'}</dd>
                </div>
                <div>
                  <dt>Estimated cost</dt>
                  <dd class:unpriced={attempt.unpriced === true}>
                    {attemptCost(attempt)}
                  </dd>
                </div>
                <div>
                  <dt>Currency</dt>
                  <dd>
                    {attempt.currency ??
                      (attempt.charge_status === 'not_billable'
                        ? 'Not applicable'
                        : 'Not recorded')}
                  </dd>
                </div>
                <div>
                  <dt>Pricing revision</dt>
                  <dd class="mono">
                    {attempt.pricing_revision_id ??
                      (attempt.charge_status === 'not_billable'
                        ? 'Not applicable'
                        : 'Not recorded')}
                  </dd>
                </div>
              </dl>
            {/if}
          </li>
        {/each}
      </ol>
    {/if}
  </section>
{/if}

<style>
  .text-button {
    padding: 0.4rem 0.65rem;
  }

  .unpriced {
    color: var(--warning);
  }

  .request-facts {
    margin-top: 1rem;
    padding: 1.5rem;
  }
  h2 {
    margin: 0;
    font-size: 1rem;
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
    padding: 1.25rem 1.25rem 1.25rem 1.5rem;
  }
  /* The marker straddles the card edge, so it needs the opaque canvas behind
     it to mask the hairline it crosses. */
  .timeline-marker {
    position: absolute;
    top: 0.85rem;
    left: -1.4rem;
    display: grid;
    width: 2rem;
    height: 2rem;
    place-items: center;
    border: 1px solid var(--border);
    border-radius: 50%;
    background: var(--canvas);
    color: var(--foreground);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-weight: 400;
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
    dl,
    .timeline dl {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
  }
  @media (max-width: 40rem) {
    dl,
    .timeline dl {
      grid-template-columns: 1fr;
    }
  }
  @media (forced-colors: active) {
    .timeline-marker {
      border: 1px solid CanvasText;
    }
  }
</style>
