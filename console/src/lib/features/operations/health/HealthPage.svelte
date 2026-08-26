<script lang="ts">
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import {
    cursorPaginationProps,
    emptyCursorHistory
  } from '$lib/api/pagination';
  import {
    acknowledgeRequestMetadataGatewayEpoch,
    getReadiness,
    listProviderHealth,
    listRequestMetadataGatewayEpochs
  } from '$lib/api/health';
  import { errorMessage } from '$lib/api/http';
  import { listRuntimeGenerations } from '$lib/api/runtime';
  import { usageCompleteness } from '$lib/api/usage';
  import { formatDate, formatInteger } from '$lib/format';
  import { spoolUsage } from './spool';
  import {
    CHECKPOINT_STALE_SECONDS,
    ageStatus,
    oldestPendingStatus,
    reportedAgeStatus
  } from './staleness';

  const generationPagination = $state(emptyCursorHistory());
  const epochPagination = $state(emptyCursorHistory());
  let busyEpoch = $state('');
  let epochNotice = $state('');
  let epochError = $state('');
  const refetchInterval = 15_000;
  // The backend accepts 1 through 1440 minutes; these are the operator-facing
  // windows worth one click.
  const windowOptions = [
    { minutes: 5, label: '5 minutes' },
    { minutes: 15, label: '15 minutes' },
    { minutes: 60, label: '1 hour' },
    { minutes: 1440, label: '24 hours' }
  ];
  let windowMinutes = $state(15);

  const readiness = createQuery(() => ({
    queryKey: ['operator-health', 'readiness'],
    queryFn: () => getReadiness(),
    refetchInterval
  }));
  const providers = createQuery(() => ({
    queryKey: ['operator-health', 'providers', windowMinutes],
    queryFn: () => listProviderHealth(windowMinutes),
    placeholderData: (previous) => previous,
    refetchInterval
  }));
  const persistence = createQuery(() => ({
    queryKey: ['operator-health', 'persistence'],
    queryFn: () => {
      const end = new Date();
      const start = new Date(end.valueOf() - 24 * 60 * 60 * 1000);
      return usageCompleteness({ start: start.toISOString(), end: end.toISOString() });
    },
    refetchInterval
  }));
  const generations = createQuery(() => ({
    queryKey: ['operator-health', 'generations', generationPagination.cursor ?? 'first'],
    queryFn: () => listRuntimeGenerations(generationPagination.cursor),
    placeholderData: (previous) => previous,
    refetchInterval
  }));
  const epochs = createQuery(() => ({
    queryKey: ['operator-health', 'epochs', epochPagination.cursor ?? 'first'],
    queryFn: () => listRequestMetadataGatewayEpochs('unresolved', epochPagination.cursor),
    placeholderData: (previous) => previous,
    refetchInterval
  }));

  const panels = [readiness, providers, persistence, generations, epochs];
  const fetching = $derived(panels.some((panel) => panel.isFetching));
  const checkedAt = $derived.by(() => {
    const stamps = panels.map((panel) => panel.dataUpdatedAt).filter((at) => at > 0);
    return stamps.length === 0 ? 0 : Math.min(...stamps);
  });

  // Ages are measured against the moment the snapshot arrived, so a paused tab
  // does not silently age every checkpoint past its threshold.
  const ages = $derived.by(() => {
    const data = readiness.data;
    const now = readiness.dataUpdatedAt || Date.now();
    return {
      planeProgress: ageStatus(data?.asynchronous_plane_last_progress_at, now),
      metadataCheckpoint: reportedAgeStatus(
        data?.request_metadata_consumer_heartbeat_age_seconds
      ),
      metadataOldestPending: oldestPendingStatus(
        data?.request_metadata_consumer_oldest_pending_at,
        data?.request_metadata_consumer_oldest_pending_age_seconds,
        now
      ),
      outboxHeartbeat: reportedAgeStatus(data?.runtime_outbox_heartbeat_age_seconds),
      // A pending outbox row is not waiting on the metadata reclaim window:
      // the publication path is stale after CHECKPOINT_STALE_SECONDS, so the
      // longer pending-recovery default would hide a wedged outbox.
      outboxOldestPending: oldestPendingStatus(
        data?.runtime_outbox_oldest_pending_at,
        data?.runtime_outbox_oldest_pending_age_seconds,
        now,
        CHECKPOINT_STALE_SECONDS
      )
    };
  });

  function refresh() {
    for (const panel of panels) void panel.refetch();
  }

  function healthTone(value?: string | null) {
    const state = value?.toLowerCase();
    if (!state) return 'warning';
    if (['healthy', 'ok', 'active', 'passing', 'drained'].includes(state)) return 'success';
    // `not_configured` is a deployment choice rather than a fault: distributed
    // limits report it when no limiter backend is configured at all, and the
    // gateway is then running exactly as installed. `unavailable` is the
    // opposite case — a limiter is configured and cannot be reached — so it
    // falls through to danger with the other hard failures.
    if (['degraded', 'stale', 'unknown', 'not_checked', 'backlogged', 'unavailable_lkg', 'not_configured'].includes(state)) return 'warning';
    return 'danger';
  }

  function stateLabel(value?: string | null) {
    return value ? value.replaceAll('_', ' ') : 'unknown';
  }

  function count(value?: number | null) {
    return formatInteger(value ?? null);
  }

  function percent(success: number, total: number) {
    return total === 0 ? 'No traffic' : `${((success / total) * 100).toFixed(1)}%`;
  }

  async function acknowledgeEpoch(processEpoch: string, gateway: string) {
    if (!window.confirm(`Acknowledge the investigated unclean epoch for ${gateway}? Retained gap evidence will not be removed.`)) return;
    busyEpoch = processEpoch;
    epochError = '';
    epochNotice = '';
    try {
      await acknowledgeRequestMetadataGatewayEpoch(processEpoch);
      epochNotice = `Epoch ${processEpoch} acknowledged. Historical completeness evidence remains retained.`;
      await Promise.all([epochs.refetch(), readiness.refetch()]);
    } catch (error) {
      epochError = errorMessage(error, 'The epoch could not be acknowledged.');
    } finally {
      busyEpoch = '';
    }
  }
</script>

<!-- Readiness fields are read by name. A field the backend adds later is
     carried by the response and simply not rendered until it is given a label
     here; nothing iterates the payload. -->
{#snippet fact(term: string, value: string, warn = false)}
  <div><dt>{term}</dt><dd class:warning-text={warn}>{value}</dd></div>
{/snippet}

{#snippet timedFact(term: string, at: string | null | undefined, age: { seconds: number | null; label: string; stale: boolean }, absent: string)}
  <div><dt>{term}</dt><dd class:warning-text={age.stale}>{age.seconds === null ? absent : age.label}{#if at}<small>{formatDate(at)}</small>{/if}</dd></div>
{/snippet}

<svelte:head><title>Health · OpenLLMProxy</title></svelte:head>

<div class="page-header">
  <div><p class="eyebrow">Operations</p><h1 class="page-title">Health</h1><p class="page-description">Gateway dependencies, worker checkpoints, provider outcomes, runtime convergence, and persistence completeness.</p></div>
  <button class="button button-secondary" type="button" onclick={refresh} disabled={fetching}>Refresh</button>
</div>

<p class="refresh-note" aria-live="polite">Automatically refreshes every 15 seconds{checkedAt ? ` · Last checked ${new Date(checkedAt).toLocaleTimeString()}` : ''}.{fetching ? ' Checking now…' : ''}</p>

{#if readiness.isError}
  <div class="inline-problem" role="alert"><strong>Control health is unavailable.</strong> {errorMessage(readiness.error, 'The control API did not respond.')} The gateway may still be serving its last-known-good runtime. <button class="text-button" onclick={() => readiness.refetch()}>Try again</button></div>
{:else if !readiness.data}
  <div class="loading-state" role="status">Checking the installation…</div>
{:else}
  <section class="metric-grid" aria-label="Dependency readiness">
    <article class="card metric-card"><p>Gateway</p><strong><span class="badge {healthTone(readiness.data.status)}">{stateLabel(readiness.data.status)}</span></strong></article>
    <article class="card metric-card"><p>PostgreSQL</p><strong><span class="badge {healthTone(readiness.data.database)}">{stateLabel(readiness.data.database)}</span></strong></article>
    <article class="card metric-card"><p>Distributed limits</p><strong><span class="badge {healthTone(readiness.data.limits)}">{stateLabel(readiness.data.limits)}</span></strong></article>
    <article class="card metric-card"><p>Active generation</p><strong>{readiness.data.generation == null ? '—' : `#${readiness.data.generation}`}</strong></article>
  </section>

  <section class="section" aria-labelledby="plane-title">
    <div class="section-heading"><div><p class="eyebrow">Replicated workers</p><h2 id="plane-title">Asynchronous plane</h2><p class="section-description">Healthy means every fixed worker task holds a current checkpoint and both the request-metadata group and the runtime outbox are drained. It does not require one specific replica. Metadata, outbox, and gateway-epoch checkpoints go stale after {CHECKPOINT_STALE_SECONDS} seconds; the maintenance task runs on a longer budget, and the stale count below is the backend's own verdict for every fixed task.</p></div><span class="badge {healthTone(readiness.data.asynchronous_plane)}">{stateLabel(readiness.data.asynchronous_plane)}</span></div>
    <dl class="card facts">
      {@render fact('Checkpoints', readiness.data.asynchronous_plane_current ? 'Current' : 'Behind', !readiness.data.asynchronous_plane_current)}
      {@render fact('Queues', readiness.data.asynchronous_plane_drained ? 'Drained' : 'Not drained', !readiness.data.asynchronous_plane_drained)}
      {@render timedFact('Last progress', readiness.data.asynchronous_plane_last_progress_at, ages.planeProgress, 'No progress recorded')}
      {@render fact('Stale task checkpoints', count(readiness.data.worker_tasks_stale), (readiness.data.worker_tasks_stale ?? 0) > 0)}
      {@render fact('Tasks that never reported', count(readiness.data.worker_tasks_unknown), (readiness.data.worker_tasks_unknown ?? 0) > 0)}
    </dl>
  </section>

  {#if persistence.isError}
    <div class="inline-problem" role="alert">{errorMessage(persistence.error, 'Usage accounting completeness is unavailable.')} <button class="text-button" onclick={() => persistence.refetch()}>Try again</button></div>
  {:else if persistence.data}
    <section class="card persistence" aria-labelledby="persistence-title">
      <div class="health-icon" class:ok={persistence.data.complete} aria-hidden="true">{persistence.data.complete ? '✓' : '!'}</div>
      <div><p class="eyebrow">Last 24 hours</p><h2 id="persistence-title">{persistence.data.complete ? 'Usage accounting is complete' : 'Usage accounting needs attention'}</h2><p>{persistence.data.request_metadata_gap_events} request metadata gap-event lower bound · {persistence.data.uncertain_request_metadata_gap_count} uncertain request metadata epochs · {persistence.data.incomplete_count} incomplete requests · {persistence.data.unpriced_count} unpriced requests. Missing or uncertain metadata is reported, never silently converted to zero cost.</p><p><a href={resolve('/usage')}>Open usage for priced totals and range coverage</a></p></div>
    </section>
  {:else}
    <div class="loading-state" role="status">Checking usage accounting…</div>
  {/if}

  <section class="section" aria-labelledby="metadata-title">
    <div class="section-heading"><div><p class="eyebrow">Request metadata durability</p><h2 id="metadata-title">Persistence pipeline</h2><p class="section-description">Content-free counters straight from readiness. Reclaims and duplicates show recovery in progress, not necessarily an incident.</p></div><span class="badge {healthTone(readiness.data.request_metadata_consumer)}">{stateLabel(readiness.data.request_metadata_consumer)}</span></div>
    <dl class="card facts">
      {@render fact('Metadata completeness', readiness.data.request_metadata_complete ? 'Complete' : 'Incomplete', !readiness.data.request_metadata_complete)}
      {@render fact('Pending acknowledgements', count(readiness.data.request_metadata_consumer_pending_events))}
      {@render fact('Stream lag', count(readiness.data.request_metadata_consumer_lag_events))}
      {@render timedFact('Oldest pending event', readiness.data.request_metadata_consumer_oldest_pending_at, ages.metadataOldestPending, 'None waiting')}
      {@render timedFact('Worker checkpoint', readiness.data.request_metadata_consumer_checked_at, ages.metadataCheckpoint, 'No checkpoint')}
      {@render fact('Reclaimed events', count(readiness.data.request_metadata_reclaimed_events_total))}
      {@render fact('Recovered events', count(readiness.data.request_metadata_recovered_events_total))}
      {@render fact('Duplicate persistence', count(readiness.data.request_metadata_duplicate_persistence_total))}
      {@render fact('Open gateway epochs', count(readiness.data.request_metadata_gateway_open_epochs))}
      {@render fact('Unresolved gateway epochs', count(readiness.data.request_metadata_gateway_unresolved_epochs), (readiness.data.request_metadata_gateway_unresolved_epochs ?? 0) > 0)}
      {@render fact('Unresolved event lower bound', count(readiness.data.request_metadata_gateway_unresolved_event_lower_bound))}
      {@render fact('Historical uncertain gaps', count(readiness.data.request_metadata_historical_uncertain_gaps))}
    </dl>
  </section>

  <section class="section" aria-labelledby="outbox-title">
    <div class="section-heading"><div><p class="eyebrow">Runtime publication</p><h2 id="outbox-title">Runtime outbox</h2><p class="section-description">A released outbox session can be replaced during the {CHECKPOINT_STALE_SECONDS}-second handoff. Inspect the PostgreSQL advisory-lock session when failed takeovers rise.</p></div><span class="badge {healthTone(readiness.data.runtime_outbox)}">{stateLabel(readiness.data.runtime_outbox)}</span></div>
    <dl class="card facts">
      {@render fact('Pending rows', count(readiness.data.runtime_outbox_pending_rows))}
      {@render fact('Claimed rows', count(readiness.data.runtime_outbox_claimed_rows))}
      {@render timedFact('Oldest pending row', readiness.data.runtime_outbox_oldest_pending_at, ages.outboxOldestPending, 'None waiting')}
      {@render fact('Owner session', readiness.data.runtime_outbox_owner_active ? 'Active' : 'None', !readiness.data.runtime_outbox_owner_active)}
      {@render fact('Ownership', readiness.data.runtime_outbox_owner_abandoned ? 'Abandoned' : 'Held', readiness.data.runtime_outbox_owner_abandoned)}
      {@render timedFact('Owner heartbeat', null, ages.outboxHeartbeat, 'No heartbeat')}
      {@render fact('Publication attempts', count(readiness.data.runtime_outbox_publication_attempts_total))}
      {@render fact('Publication retries', count(readiness.data.runtime_outbox_publication_retries_total))}
      {@render fact('Repeated publications', count(readiness.data.runtime_outbox_repeated_publication_attempts_total))}
      {@render fact('Abandoned ownerships', count(readiness.data.runtime_outbox_abandoned_ownership_total))}
      {@render fact('Failed takeovers', count(readiness.data.runtime_outbox_failed_takeovers_total), (readiness.data.runtime_outbox_failed_takeovers_total ?? 0) > 0)}
    </dl>
  </section>

  <section class="section" aria-labelledby="media-title">
    <div class="section-heading"><div><p class="eyebrow">Asynchronous media</p><h2 id="media-title">Media reconciliation</h2><p class="section-description">Lifecycle bookkeeping for asynchronous media jobs. A gap is a job whose upstream outcome could not be established.</p></div><span class="badge {healthTone(readiness.data.media_reconciliation)}">{stateLabel(readiness.data.media_reconciliation)}</span></div>
    <dl class="card facts">
      {@render fact('Pending', count(readiness.data.media_reconciliation_pending))}
      {@render fact('Stale', count(readiness.data.media_reconciliation_stale), (readiness.data.media_reconciliation_stale ?? 0) > 0)}
      {@render fact('Failed', count(readiness.data.media_reconciliation_failed), (readiness.data.media_reconciliation_failed ?? 0) > 0)}
      {@render fact('Unbound', count(readiness.data.media_reconciliation_unbound))}
      {@render fact('Recorded gaps', count(readiness.data.media_reconciliation_gaps_total), (readiness.data.media_reconciliation_gaps_total ?? 0) > 0)}
      {@render fact('Media spool', spoolUsage(readiness.data.media_spool_used_bytes, readiness.data.media_spool_capacity_bytes))}
    </dl>
    <p class="section-link"><a href={resolve('/media-jobs')}>Open media jobs</a></p>
  </section>

  <section class="section" aria-labelledby="epochs-title">
    <div class="section-heading"><div><p class="eyebrow">Request metadata durability</p><h2 id="epochs-title">Unresolved gateway epochs</h2><p class="section-description">An unclean process epoch keeps readiness degraded until an operator investigates and acknowledges it. Acknowledgement is audited and never deletes its retained loss or uncertainty evidence.</p></div>{#if epochs.data}<span class:warning={epochs.data.items.length > 0} class:success={epochs.data.items.length === 0} class="badge">{epochs.data.items.length} on page</span>{/if}</div>
    {#if epochNotice}<div class="inline-notice" role="status">{epochNotice}</div>{/if}
    {#if epochError}<div class="inline-problem" role="alert">{epochError}</div>{/if}
    {#if epochs.isError}
      <div class="inline-problem" role="alert">{errorMessage(epochs.error, 'Gateway epochs are unavailable.')} <button class="text-button" onclick={() => epochs.refetch()}>Try again</button></div>
    {:else if !epochs.data}
      <div class="loading-state" role="status">Loading gateway epochs…</div>
    {:else if epochs.data.items.length === 0 && epochPagination.history.length === 0}
      <div class="card empty-state">No unclean gateway epoch awaits acknowledgement.</div>
    {:else}
      <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
      <div class="table-shell" tabindex="0" role="region" aria-label="Unresolved request metadata gateway epochs"><table class="data-table"><caption class="sr-only">Unclean request metadata gateway process epochs awaiting operator acknowledgement</caption><thead><tr><th scope="col">Gateway</th><th scope="col">Lifecycle</th><th scope="col">Writer</th><th scope="col">Accepted / persisted</th><th scope="col">Dropped / abandoned</th><th scope="col">Uncertain lower bound</th><th scope="col">Acknowledged</th><th scope="col"><span class="sr-only">Action</span></th></tr></thead><tbody>{#each epochs.data.items as epoch (epoch.process_epoch)}<tr><td><strong>{epoch.gateway_instance}</strong><code>{epoch.process_epoch}</code></td><td>Started {formatDate(epoch.started_at)}<small>Detected {formatDate(epoch.stale_detected_at ?? epoch.updated_at)}</small><small>{epoch.gracefully_closed_at ? `Closed ${formatDate(epoch.gracefully_closed_at)}` : 'Never closed gracefully'}</small></td><td><span class="badge {epoch.retrying ? 'warning' : ''}">{epoch.retrying ? 'Retrying' : 'Not retrying'}</span><small>{epoch.writer_closed ? 'Writer closed' : 'Writer open'}</small></td><td>{epoch.accepted} / {epoch.persisted}</td><td>{epoch.dropped} / {epoch.abandoned}</td><td>{epoch.uncertain_event_lower_bound}</td><td>{epoch.acknowledged_at ? formatDate(epoch.acknowledged_at) : 'Not acknowledged'}{#if epoch.acknowledged_by}<small class="mono">{epoch.acknowledged_by}</small>{/if}</td><td><button class="button button-secondary" type="button" onclick={() => acknowledgeEpoch(epoch.process_epoch, epoch.gateway_instance)} disabled={Boolean(busyEpoch)}>{busyEpoch === epoch.process_epoch ? 'Acknowledging…' : 'Acknowledge epoch'}</button></td></tr>{/each}</tbody></table></div>
      <CursorPagination {...cursorPaginationProps(epochPagination, epochs.isPlaceholderData ? null : epochs.data.nextCursor)} label="Unresolved gateway epoch pages" />
    {/if}
  </section>

  <section class="section" aria-labelledby="providers-title">
    <div class="section-heading"><div><p class="eyebrow">Rolling window</p><h2 id="providers-title">Providers</h2></div><div class="heading-controls"><label class="window-select" for="provider-window">Window <select id="provider-window" bind:value={windowMinutes}>{#each windowOptions as option (option.minutes)}<option value={option.minutes}>{option.label}</option>{/each}</select></label>{#if providers.data}<span class="badge">{providers.data.data.length} configured</span>{/if}</div></div>
    {#if providers.isError}
      <div class="inline-problem" role="alert">{errorMessage(providers.error, 'Provider outcomes are unavailable.')} <button class="text-button" onclick={() => providers.refetch()}>Try again</button></div>
    {:else if !providers.data}
      <div class="loading-state" role="status">Loading provider outcomes…</div>
    {:else if providers.data.data.length === 0}
      <div class="card empty-state">No providers are configured.</div>
    {:else}
      <div class="provider-grid">
        {#each providers.data.data as provider (provider.provider_id)}
          <article class="card provider-card">
            <div class="provider-heading"><div><h3>{provider.provider_name}</h3><p>{provider.provider_kind} · {provider.provider_state}</p></div><span class="badge {healthTone(provider.status)}">{provider.status}</span></div>
            <dl><div><dt>Success rate</dt><dd>{percent(provider.success_count, provider.attempt_count)}</dd></div><div><dt>Average latency</dt><dd>{provider.average_latency_ms == null ? '—' : `${provider.average_latency_ms.toFixed(0)} ms`}</dd></div><div><dt>Rate limited</dt><dd>{provider.rate_limit_count}</dd></div><div><dt>5xx / transport</dt><dd>{provider.server_error_count} / {provider.transport_error_count}</dd></div></dl>
            <p class="probe"><strong>Last probe:</strong> {provider.last_probe_detail ?? provider.last_probe_status ?? 'Not probed'}<br /><span>{formatDate(provider.last_probe_at)}</span><br /><strong>Last live attempt:</strong> <span>{provider.last_attempt_at ? formatDate(provider.last_attempt_at) : 'No traffic in this window'}</span></p>
          </article>
        {/each}
      </div>
      <p class="section-link">Counted over the last {providers.data.window_minutes} {providers.data.window_minutes === 1 ? 'minute' : 'minutes'}. Provider probe failures stay separate from gateway admission failures.</p>
    {/if}
  </section>

  <section class="section" aria-labelledby="runtime-title">
    <div class="section-heading"><div><p class="eyebrow">Configuration</p><h2 id="runtime-title">Runtime generations</h2></div></div>
    {#if generations.isError}
      <div class="inline-problem" role="alert">{errorMessage(generations.error, 'Runtime generations are unavailable.')} <button class="text-button" onclick={() => generations.refetch()}>Try again</button></div>
    {:else if !generations.data}
      <div class="loading-state" role="status">Loading runtime generations…</div>
    {:else}
      <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
      <div class="table-shell" tabindex="0" role="region" aria-label="Runtime generation history"><table class="data-table"><caption class="sr-only">Recently published immutable runtime generations</caption><thead><tr><th scope="col">Generation</th><th scope="col">Digest</th><th scope="col">Activated by</th><th scope="col">Created</th><th scope="col">Gateway state</th></tr></thead><tbody>{#each generations.data.items as generation (generation.id)}<tr><td><strong>#{generation.sequence}</strong></td><td class="mono">{generation.sha256.slice(0, 16)}…</td><td>{generation.created_by_email}</td><td>{formatDate(generation.created_at)}</td><td>{#if generation.sequence === readiness.data.generation}<span class="badge success">Loaded</span>{:else}<span class="badge">Historical</span>{/if}</td></tr>{/each}</tbody></table></div>
      <CursorPagination {...cursorPaginationProps(generationPagination, generations.isPlaceholderData ? null : generations.data.nextCursor)} label="Runtime generation pages" />
    {/if}
  </section>
{/if}

<style>
  .refresh-note { margin: 1rem 0 0; color: var(--foreground-muted); font-size: 0.75rem; }
  .text-button { min-height: 2.75rem; border: 0; background: transparent; color: var(--accent-strong); font-weight: 700; }
  .persistence { display: flex; align-items: flex-start; gap: 1rem; margin-top: 1rem; padding: 1.25rem; }
  .health-icon { display: grid; width: 2.5rem; height: 2.5rem; flex: none; place-items: center; border-radius: 0.375rem; background: var(--danger-soft); color: var(--danger); font-weight: 900; }
  .health-icon.ok { background: var(--success-soft); color: var(--success); }
  h2, h3 { margin: 0; letter-spacing: -0.025em; }
  h2 { font-size: 1.2rem; }
  h3 { font-size: 1rem; }
  .persistence p:last-child { margin: 0.35rem 0 0; color: var(--foreground-muted); }
  .section { margin-top: 2rem; }
  .section-description { max-width: 58rem; margin: .35rem 0 0; color: var(--foreground-muted); font-size: .8rem; }
  .section-link { margin: .6rem 0 0; color: var(--foreground-muted); font-size: .78rem; }
  code { font: .72rem 'JetBrains Mono Variable', monospace; overflow-wrap: anywhere; }
  .section-heading, .provider-heading { display: flex; align-items: flex-start; justify-content: space-between; gap: 1rem; margin-bottom: 0.75rem; }
  .heading-controls { display: flex; flex: none; align-items: center; gap: .6rem; }
  .window-select { display: grid; gap: .25rem; color: var(--foreground-muted); font-size: .7rem; font-weight: 700; }
  /* The label is deliberately small and bold; the chosen window is content and
     keeps a readable control size. */
  .window-select select { min-height: 2.5rem; padding: .35rem .6rem; border: 1px solid var(--border-strong); border-radius: .375rem; background: var(--surface); color: var(--foreground); font-size: .8125rem; font-weight: 600; }
  .facts { display: grid; grid-template-columns: repeat(auto-fit, minmax(11rem, 1fr)); gap: 1rem; margin: 0; padding: 1.1rem 1.25rem; }
  .facts div { min-width: 0; }
  .provider-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 0.85rem; }
  .provider-card { padding: 1rem; }
  .provider-heading p { margin: 0.15rem 0 0; color: var(--foreground-muted); font-size: 0.75rem; }
  dl { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 0.75rem; margin: 1rem 0 0; }
  dt { color: var(--foreground-muted); font-size: 0.7rem; font-weight: 700; }
  dd { margin: 0.1rem 0 0; font-weight: 700; overflow-wrap: anywhere; }
  dd small, td small, td code { display: block; margin-top: .15rem; color: var(--foreground-muted); font-size: .7rem; font-weight: 500; }
  .warning-text { color: var(--warning); }
  .probe { margin: 1rem 0 0; padding-top: 0.8rem; border-top: 1px solid var(--border); color: var(--foreground-muted); font-size: 0.75rem; overflow-wrap: anywhere; }
  @media (max-width: 60rem) { .provider-grid { grid-template-columns: 1fr; } }
  @media (max-width: 36rem) { .persistence { display: grid; } dl { grid-template-columns: 1fr; } .section-heading { display: grid; } }
</style>
