<script lang="ts">
  import { resolve } from '$app/paths';
  import {
    formatCost,
    formatDate,
    formatInteger,
    statusLabel,
    statusTone
  } from '#lib/format.js';
  import type { RequestAttempt } from '#lib/features/usage/history/api.js';
  import ProviderRequestID from './ProviderRequestID.svelte';
  import AttemptRouting from './AttemptRouting.svelte';

  let { attempt }: { attempt: RequestAttempt } = $props();

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

<li class="card">
  <span class="timeline-marker" aria-hidden="true">{attempt.ordinal}</span>
  <div class="attempt-heading">
    <div>
      <strong
        ><a
          class="provider-link"
          href={resolve(`providers/${attempt.provider_id}`)}
          >{attempt.provider_name}</a
        >
      </strong>
      <span class="mono">{attempt.upstream_model}</span>
    </div>
    <span class="badge {statusTone(attempt.status_code, attempt.error_class)}"
      >{statusLabel(attempt.status_code, attempt.error_class)}</span
    >
  </div>
  <dl>
    {#if attempt.provider_request_id}
      <div class="provider-request-id">
        <dt>Provider request ID</dt>
        <dd>
          <ProviderRequestID id={attempt.provider_request_id} />
        </dd>
      </div>
    {/if}
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
        {attempt.first_byte_ms === null || attempt.first_byte_ms === undefined
          ? '—'
          : `${attempt.first_byte_ms} ms`}
      </dd>
    </div>
    <div>
      <dt>Latency</dt>
      <dd>
        {attempt.latency_ms === null || attempt.latency_ms === undefined
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
    <AttemptRouting routing={attempt.routing} />
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
        <dt>Cache write tokens</dt>
        <dd>
          {formatInteger(attempt.cache_write_input_tokens)}
          {#if attempt.cache_write_5m_input_tokens || attempt.cache_write_1h_input_tokens}
            <small
              >5m {formatInteger(attempt.cache_write_5m_input_tokens)} · 1h {formatInteger(
                attempt.cache_write_1h_input_tokens
              )}</small
            >
          {/if}
        </dd>
      </div>
      <div>
        <dt>Output tokens</dt>
        <dd>{formatInteger(attempt.output_tokens)}</dd>
      </div>
      {#if attempt.audio_input_tokens != null || attempt.cached_audio_input_tokens != null || attempt.audio_output_tokens != null || attempt.image_input_tokens != null || attempt.cached_image_input_tokens != null || attempt.image_output_tokens != null}
        <div>
          <dt>Audio tokens</dt>
          <dd>
            {formatInteger(attempt.audio_input_tokens)} in · {formatInteger(
              attempt.audio_output_tokens
            )} out{#if attempt.cached_audio_input_tokens != null}
              <small
                >{formatInteger(attempt.cached_audio_input_tokens)} cached input</small
              >{/if}
          </dd>
        </div>
        <div>
          <dt>Image tokens</dt>
          <dd>
            {formatInteger(attempt.image_input_tokens)} in · {formatInteger(
              attempt.image_output_tokens
            )} out{#if attempt.cached_image_input_tokens != null}
              <small
                >{formatInteger(attempt.cached_image_input_tokens)} cached input</small
              >{/if}
          </dd>
        </div>
      {/if}
      {#if attempt.tool_calls != null && Object.keys(attempt.tool_calls).length > 0}
        <div>
          <dt>Tool calls</dt>
          <dd>
            {#each Object.entries(attempt.tool_calls) as [tool, count] (tool)}
              <small>{tool} × {formatInteger(count)}</small>
            {/each}
          </dd>
        </div>
      {/if}
      {#if attempt.batch === true}
        <div>
          <dt>Batch pricing</dt>
          <dd>Yes — settled at batch terms</dd>
        </div>
      {/if}
      {#if attempt.pricing_components && attempt.pricing_components.length > 0}
        <div>
          <dt>Priced components</dt>
          <dd class="mono">
            {attempt.pricing_components.join(', ')}
          </dd>
        </div>
      {/if}
      {#if attempt.commitment_amount != null}
        <div>
          <dt>Commitment</dt>
          <dd>
            {formatCost(attempt.commitment_amount, attempt.currency)} per
            {attempt.commitment_period}
            <small>
              List-cost weight {attempt.commitment_weight ?? '—'}; marginal cost
              is zero.
            </small>
          </dd>
        </div>
      {/if}
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
      <div>
        <dt>Charge</dt>
        <dd>
          {attempt.charge != null
            ? formatCost(attempt.charge, attempt.currency)
            : attempt.charge_status === 'not_billable'
              ? 'Not applicable'
              : 'Not recorded'}
        </dd>
      </div>
      <div>
        <dt>Rate card revision</dt>
        <dd class="mono">
          {attempt.rate_card_revision_id ??
            (attempt.charge != null && attempt.rate_card_revision_id == null
              ? 'No card'
              : 'Not recorded')}
        </dd>
      </div>
      <div>
        <dt>Discount</dt>
        <dd class="mono">{attempt.discount_id ?? 'None'}</dd>
      </div>
    </dl>
  {/if}
</li>

<style>
  li {
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
  .unpriced {
    color: var(--warning);
  }
  .attempt-heading strong,
  .attempt-heading .mono {
    display: block;
  }
  .attempt-heading .mono {
    margin-top: 0.15rem;
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  .provider-link {
    text-decoration: underline;
    text-decoration-color: var(--border-strong);
    text-underline-offset: 4px;
    transition: text-decoration-color var(--motion);
  }
  .provider-link:hover {
    text-decoration-color: currentColor;
  }
  dl {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(9rem, 1fr));
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
  @media (forced-colors: active) {
    .timeline-marker {
      border: 1px solid CanvasText;
    }
  }
</style>
