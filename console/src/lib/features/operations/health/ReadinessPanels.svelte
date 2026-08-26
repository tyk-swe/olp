<script lang="ts">
  import { resolve } from '$app/paths';
  import type { CreateQueryResult } from '@tanstack/svelte-query';
  import type { Readiness } from '$lib/api/health';
  import { errorMessage } from '$lib/api/http';
  import type { UsageCompleteness } from '$lib/api/usage';
  import { formatDate, formatInteger } from '$lib/format';
  import { healthTone, stateLabel } from './presentation';
  import { spoolUsage } from './spool';
  import {
    CHECKPOINT_STALE_SECONDS,
    ageStatus,
    oldestPendingStatus,
    reportedAgeStatus
  } from './staleness';

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
      ),
      outboxHeartbeat: reportedAgeStatus(
        readiness.runtime_outbox_heartbeat_age_seconds
      ),
      // A pending outbox row is not waiting on the metadata reclaim window:
      // the publication path is stale after CHECKPOINT_STALE_SECONDS, so the
      // longer pending-recovery default would hide a wedged outbox.
      outboxOldestPending: oldestPendingStatus(
        readiness.runtime_outbox_oldest_pending_at,
        readiness.runtime_outbox_oldest_pending_age_seconds,
        now,
        CHECKPOINT_STALE_SECONDS
      )
    };
  });

  function count(value?: number | null) {
    return formatInteger(value ?? null);
  }
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
        Healthy means every fixed worker task holds a current checkpoint and
        both the request-metadata group and the runtime outbox are drained. It
        does not require one specific replica. Metadata, outbox, and
        gateway-epoch checkpoints go stale after {CHECKPOINT_STALE_SECONDS} seconds;
        the maintenance task runs on a longer budget, and the stale count below is
        the backend's own verdict for every fixed task.
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
      count(readiness.worker_tasks_stale),
      (readiness.worker_tasks_stale ?? 0) > 0
    )}
    {@render fact(
      'Tasks that never reported',
      count(readiness.worker_tasks_unknown),
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
        <a href={resolve('/usage')}
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
      count(readiness.request_metadata_consumer_pending_events)
    )}
    {@render fact(
      'Stream lag',
      count(readiness.request_metadata_consumer_lag_events)
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
      count(readiness.request_metadata_reclaimed_events_total)
    )}
    {@render fact(
      'Recovered events',
      count(readiness.request_metadata_recovered_events_total)
    )}
    {@render fact(
      'Duplicate persistence',
      count(readiness.request_metadata_duplicate_persistence_total)
    )}
    {@render fact(
      'Open gateway epochs',
      count(readiness.request_metadata_gateway_open_epochs)
    )}
    {@render fact(
      'Unresolved gateway epochs',
      count(readiness.request_metadata_gateway_unresolved_epochs),
      (readiness.request_metadata_gateway_unresolved_epochs ?? 0) > 0
    )}
    {@render fact(
      'Unresolved event lower bound',
      count(readiness.request_metadata_gateway_unresolved_event_lower_bound)
    )}
    {@render fact(
      'Historical uncertain gaps',
      count(readiness.request_metadata_historical_uncertain_gaps)
    )}
  </dl>
</section>

<section class="section" aria-labelledby="outbox-title">
  <div class="section-heading">
    <div>
      <p class="eyebrow">Runtime publication</p>
      <h2 id="outbox-title">Runtime outbox</h2>
      <p class="section-description">
        A released outbox session can be replaced during the {CHECKPOINT_STALE_SECONDS}-second
        handoff. Inspect the PostgreSQL advisory-lock session when failed
        takeovers rise.
      </p>
    </div>
    <span class="badge {healthTone(readiness.runtime_outbox)}"
      >{stateLabel(readiness.runtime_outbox)}</span
    >
  </div>
  <dl class="card facts">
    {@render fact('Pending rows', count(readiness.runtime_outbox_pending_rows))}
    {@render fact('Claimed rows', count(readiness.runtime_outbox_claimed_rows))}
    {@render timedFact(
      'Oldest pending row',
      readiness.runtime_outbox_oldest_pending_at,
      ages.outboxOldestPending,
      'None waiting'
    )}
    {@render fact(
      'Owner session',
      readiness.runtime_outbox_owner_active ? 'Active' : 'None',
      !readiness.runtime_outbox_owner_active
    )}
    {@render fact(
      'Ownership',
      readiness.runtime_outbox_owner_abandoned ? 'Abandoned' : 'Held',
      readiness.runtime_outbox_owner_abandoned
    )}
    {@render timedFact(
      'Owner heartbeat',
      null,
      ages.outboxHeartbeat,
      'No heartbeat'
    )}
    {@render fact(
      'Publication attempts',
      count(readiness.runtime_outbox_publication_attempts_total)
    )}
    {@render fact(
      'Publication retries',
      count(readiness.runtime_outbox_publication_retries_total)
    )}
    {@render fact(
      'Repeated publications',
      count(readiness.runtime_outbox_repeated_publication_attempts_total)
    )}
    {@render fact(
      'Abandoned ownerships',
      count(readiness.runtime_outbox_abandoned_ownership_total)
    )}
    {@render fact(
      'Failed takeovers',
      count(readiness.runtime_outbox_failed_takeovers_total),
      (readiness.runtime_outbox_failed_takeovers_total ?? 0) > 0
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
    {@render fact('Pending', count(readiness.media_reconciliation_pending))}
    {@render fact(
      'Stale',
      count(readiness.media_reconciliation_stale),
      (readiness.media_reconciliation_stale ?? 0) > 0
    )}
    {@render fact(
      'Failed',
      count(readiness.media_reconciliation_failed),
      (readiness.media_reconciliation_failed ?? 0) > 0
    )}
    {@render fact('Unbound', count(readiness.media_reconciliation_unbound))}
    {@render fact(
      'Recorded gaps',
      count(readiness.media_reconciliation_gaps_total),
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
    <a href={resolve('/media-jobs')}>Open media jobs</a>
  </p>
</section>

<style>
  .text-button {
    min-height: 2.75rem;
    border: 0;
    background: transparent;
    color: var(--accent-strong);
    font-weight: 700;
  }
  .persistence {
    display: flex;
    align-items: flex-start;
    gap: 1rem;
    margin-top: 1rem;
    padding: 1.25rem;
  }
  .health-icon {
    display: grid;
    width: 2.5rem;
    height: 2.5rem;
    flex: none;
    place-items: center;
    border-radius: 0.375rem;
    background: var(--danger-soft);
    color: var(--danger);
    font-weight: 900;
  }
  .health-icon.ok {
    background: var(--success-soft);
    color: var(--success);
  }
  h2 {
    margin: 0;
    font-size: 1.2rem;
    letter-spacing: -0.025em;
  }
  .persistence p:last-child {
    margin: 0.35rem 0 0;
    color: var(--foreground-muted);
  }
  .section {
    margin-top: 2rem;
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
  .section-heading {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 1rem;
    margin-bottom: 0.75rem;
  }
  .facts {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(11rem, 1fr));
    gap: 1rem;
    margin: 0;
    padding: 1.1rem 1.25rem;
  }
  .facts div {
    min-width: 0;
  }
  dt {
    color: var(--foreground-muted);
    font-size: 0.7rem;
    font-weight: 700;
  }
  dd {
    margin: 0.1rem 0 0;
    font-weight: 700;
    overflow-wrap: anywhere;
  }
  dd small {
    display: block;
    margin-top: 0.15rem;
    color: var(--foreground-muted);
    font-size: 0.7rem;
    font-weight: 500;
  }
  .warning-text {
    color: var(--warning);
  }
  @media (max-width: 36rem) {
    .persistence {
      display: grid;
    }
    .section-heading {
      display: grid;
    }
  }
</style>
