<script lang="ts">
  import { resolve } from '$app/paths';
  import type { CreateQueryResult } from '@tanstack/svelte-query';
  import type { Readiness } from '#lib/features/runtime/health/api.js';
  import { errorMessage } from '#lib/management/http.js';
  import type { UsageCompleteness } from '#lib/features/usage/usage.js';
  import { formatDate, formatInteger, stateLabel } from '#lib/format.js';
  import { healthTone } from '#lib/features/runtime/health/presentation.js';
  import { spoolUsage } from '#lib/features/runtime/health/spool.js';
  import {
    CHECKPOINT_STALE_SECONDS,
    ageStatus,
    oldestPendingStatus,
    reportedAgeStatus
  } from '#lib/features/runtime/health/staleness.js';

  let {
    readiness,
    observedAt,
    persistence
  }: {
    readiness: Readiness;
    observedAt: number;
    persistence: CreateQueryResult<UsageCompleteness>;
  } = $props();

  // Ages are measured against the moment the snapshot arrived, so a paused tab
  // does not silently age every checkpoint past its threshold.
  const ages = $derived.by(() => {
    const now = observedAt || Date.now();
    return {
      planeProgress: ageStatus(
        readiness.asynchronous_plane_last_progress_at,
        now
      ),
      metadataCheckpoint: reportedAgeStatus(
        readiness.request_metadata_consumer_heartbeat_age_seconds
      ),
      metadataOldestPending: oldestPendingStatus(
        readiness.request_metadata_consumer_oldest_pending_at,
        readiness.request_metadata_consumer_oldest_pending_age_seconds,
        now
      )
    };
  });
</script>

<!-- Readiness fields are read by name. A field the backend adds later is
     carried by the response and simply not rendered until it is given a label
     here; nothing iterates the payload. -->
{#snippet fact(term: string, value: string, warn = false)}
  <div>
    <dt>{term}</dt>
    <dd class:warning-text={warn}>{value}</dd>
  </div>
{/snippet}

{#snippet timedFact(
  term: string,
  at: string | null | undefined,
  age: { seconds: number | null; label: string; stale: boolean },
  absent: string
)}
  <div>
    <dt>{term}</dt>
    <dd class:warning-text={age.stale}>
      {age.seconds === null ? absent : age.label}{#if at}<small
          >{formatDate(at)}</small
        >{/if}
    </dd>
  </div>
{/snippet}

<section class="metric-grid" aria-label="Dependency readiness">
  <article class="card metric-card">
    <p>Gateway</p>
    <strong
      ><span class="badge {healthTone(readiness.status)}"
        >{stateLabel(readiness.status)}</span
      ></strong
    >
  </article>
  <article class="card metric-card">
    <p>PostgreSQL</p>
    <strong
      ><span class="badge {healthTone(readiness.database)}"
        >{stateLabel(readiness.database)}</span
      ></strong
    >
  </article>
  <article class="card metric-card">
    <p>Distributed limits</p>
    <strong
      ><span class="badge {healthTone(readiness.limits)}"
        >{stateLabel(readiness.limits)}</span
      ></strong
    >
  </article>
  <article class="card metric-card">
    <p>Active generation</p>
    <strong
      >{readiness.generation == null ? '—' : `#${readiness.generation}`}</strong
    >
  </article>
</section>

<section class="section" aria-labelledby="plane-title">
  <div class="section-heading">
    <div>
      <p class="eyebrow">Replicated workers</p>
      <h2 id="plane-title">Asynchronous plane</h2>
      <p class="section-description">
        Healthy means every fixed worker task holds a current checkpoint and the
        request-metadata group is drained. It does not require one specific
        replica. Metadata and gateway-epoch checkpoints go stale after {CHECKPOINT_STALE_SECONDS}
        seconds; the maintenance task runs on a longer budget, and the stale count
        below is the backend's own verdict for every fixed task.
      </p>
    </div>
    <span class="badge {healthTone(readiness.asynchronous_plane)}"
      >{stateLabel(readiness.asynchronous_plane)}</span
    >
  </div>
  <dl class="card facts">
    {@render fact(
      'Checkpoints',
      readiness.asynchronous_plane_current ? 'Current' : 'Behind',
      !readiness.asynchronous_plane_current
    )}
    {@render fact(
      'Queues',
      readiness.asynchronous_plane_drained ? 'Drained' : 'Not drained',
      !readiness.asynchronous_plane_drained
    )}
    {@render timedFact(
      'Last progress',
      readiness.asynchronous_plane_last_progress_at,
      ages.planeProgress,
      'No progress recorded'
    )}
    {@render fact(
      'Stale task checkpoints',
      formatInteger(readiness.worker_tasks_stale),
      (readiness.worker_tasks_stale ?? 0) > 0
    )}
    {@render fact(
      'Tasks that never reported',
      formatInteger(readiness.worker_tasks_unknown),
      (readiness.worker_tasks_unknown ?? 0) > 0
    )}
  </dl>
</section>

{#if persistence.isError}
  <div class="inline-problem" role="alert">
    {errorMessage(
      persistence.error,
      'Usage accounting completeness is unavailable.'
    )}
    <button class="text-button" onclick={() => persistence.refetch()}
      >Try again</button
    >
  </div>
{:else if persistence.data}
  <section class="card persistence" aria-labelledby="persistence-title">
    <div
      class="health-icon"
      class:ok={persistence.data.complete}
      aria-hidden="true"
    >
      {persistence.data.complete ? '✓' : '!'}
    </div>
    <div>
      <p class="eyebrow">Last 24 hours</p>
      <h2 id="persistence-title">
        {persistence.data.complete
          ? 'Usage accounting is complete'
          : 'Usage accounting needs attention'}
      </h2>
      <p>
        {persistence.data.request_metadata_gap_events} request metadata gap-event
        lower bound · {persistence.data.uncertain_request_metadata_gap_count} uncertain
        request metadata epochs · {persistence.data.incomplete_count} incomplete requests
        · {persistence.data.unpriced_count} unpriced requests. Missing or uncertain
        metadata is reported, never silently converted to zero cost.
      </p>

      <p>
        <a href={resolve('usage')}
          >Open usage for priced totals and range coverage</a
        >
      </p>
    </div>
  </section>
{:else}
  <div class="loading-state" role="status">Checking usage accounting…</div>
{/if}

<section class="section" aria-labelledby="metadata-title">
  <div class="section-heading">
    <div>
      <p class="eyebrow">Request metadata durability</p>
      <h2 id="metadata-title">Persistence pipeline</h2>
      <p class="section-description">
        Content-free counters straight from readiness. Reclaims and duplicates
        show recovery in progress, not necessarily an incident.
      </p>
    </div>
    <span class="badge {healthTone(readiness.request_metadata_consumer)}"
      >{stateLabel(readiness.request_metadata_consumer)}</span
    >
  </div>
  <dl class="card facts">
    {@render fact(
      'Metadata completeness',
      readiness.request_metadata_complete ? 'Complete' : 'Incomplete',
      !readiness.request_metadata_complete
    )}
    {@render fact(
      'Pending acknowledgements',
      formatInteger(readiness.request_metadata_consumer_pending_events)
    )}
    {@render fact(
      'Stream lag',
      formatInteger(readiness.request_metadata_consumer_lag_events)
    )}
    {@render timedFact(
      'Oldest pending event',
      readiness.request_metadata_consumer_oldest_pending_at,
      ages.metadataOldestPending,
      'None waiting'
    )}
    {@render timedFact(
      'Worker checkpoint',
      readiness.request_metadata_consumer_checked_at,
      ages.metadataCheckpoint,
      'No checkpoint'
    )}
    {@render fact(
      'Reclaimed events',
      formatInteger(readiness.request_metadata_reclaimed_events_total)
    )}
    {@render fact(
      'Recovered events',
      formatInteger(readiness.request_metadata_recovered_events_total)
    )}
    {@render fact(
      'Duplicate persistence',
      formatInteger(readiness.request_metadata_duplicate_persistence_total)
    )}
    {@render fact(
      'Open gateway epochs',
      formatInteger(readiness.request_metadata_gateway_open_epochs)
    )}
    {@render fact(
      'Unresolved gateway epochs',
      formatInteger(readiness.request_metadata_gateway_unresolved_epochs),
      (readiness.request_metadata_gateway_unresolved_epochs ?? 0) > 0
    )}
    {@render fact(
      'Unresolved event lower bound',
      formatInteger(
        readiness.request_metadata_gateway_unresolved_event_lower_bound
      )
    )}
    {@render fact(
      'Historical uncertain gaps',
      formatInteger(readiness.request_metadata_historical_uncertain_gaps)
    )}
  </dl>
</section>

<section class="section" aria-labelledby="media-title">
  <div class="section-heading">
    <div>
      <p class="eyebrow">Asynchronous media</p>
      <h2 id="media-title">Media reconciliation</h2>
      <p class="section-description">
        Lifecycle bookkeeping for asynchronous media jobs. A gap is a job whose
        upstream outcome could not be established.
      </p>
    </div>
    <span class="badge {healthTone(readiness.media_reconciliation)}"
      >{stateLabel(readiness.media_reconciliation)}</span
    >
  </div>
  <dl class="card facts">
    {@render fact(
      'Pending',
      formatInteger(readiness.media_reconciliation_pending)
    )}
    {@render fact(
      'Stale',
      formatInteger(readiness.media_reconciliation_stale),
      (readiness.media_reconciliation_stale ?? 0) > 0
    )}
    {@render fact(
      'Failed',
      formatInteger(readiness.media_reconciliation_failed),
      (readiness.media_reconciliation_failed ?? 0) > 0
    )}
    {@render fact(
      'Recorded gaps',
      formatInteger(readiness.media_reconciliation_gaps_total),
      (readiness.media_reconciliation_gaps_total ?? 0) > 0
    )}
    {@render fact(
      'Media spool',
      spoolUsage(
        readiness.media_spool_used_bytes,
        readiness.media_spool_capacity_bytes
      )
    )}
  </dl>

  <p class="section-link">
    <a href={resolve('media-jobs')}>Open media jobs</a>
  </p>
</section>

<style>
  .persistence {
    display: flex;
    align-items: flex-start;
    gap: 1rem;
    margin-top: 1rem;
    padding: 1.5rem;
  }
  .health-icon {
    display: grid;
    width: 2.5rem;
    height: 2.5rem;
    flex: none;
    place-items: center;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--danger);
    font-weight: 500;
  }
  .health-icon.ok {
    color: var(--success);
  }
  .persistence h2 {
    margin: 0;
    font-size: 1rem;
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .persistence p:last-child {
    margin: 0.35rem 0 0;
    color: var(--foreground-muted);
  }
  .section-description {
    max-width: 58rem;
    margin: 0.35rem 0 0;
    color: var(--foreground-muted);
    font-size: 0.8rem;
  }
  .section-link {
    margin: 0.6rem 0 0;
    color: var(--foreground-muted);
    font-size: 0.78rem;
  }
  .facts {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(11rem, 1fr));
    gap: 1rem;
    margin: 0;
    padding: 1.25rem 1.5rem;
  }
  .facts div {
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
    margin: 0.25rem 0 0;
    font-weight: 400;
    overflow-wrap: anywhere;
  }
  dd small {
    display: block;
    margin-top: 0.15rem;
    color: var(--foreground-muted);
    font-size: var(--text-caption);
    font-weight: 400;
  }
  .warning-text {
    color: var(--warning);
  }
  @media (max-width: 36rem) {
    .persistence {
      display: grid;
    }
  }
</style>
