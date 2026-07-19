<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import {
    acknowledgeUsageGatewayEpoch,
    getReadiness,
    listProviderHealth,
    listRuntimeGenerations,
    listUsageGatewayEpochs,
    usageCompleteness
  } from '$lib/api/operations';
  import { formatDate } from './format';

  let generationCursor = $state<string | undefined>();
  let generationHistory = $state<Array<string | undefined>>([]);
  let epochCursor = $state<string | undefined>();
  let epochHistory = $state<Array<string | undefined>>([]);
  let busyEpoch = $state('');
  let epochNotice = $state('');
  let epochError = $state('');
  const health = createQuery(() => ({
    queryKey: ['operator-health', generationCursor ?? 'first', epochCursor ?? 'first'],
    queryFn: async () => {
      const end = new Date();
      const start = new Date(end.valueOf() - 24 * 60 * 60 * 1000);
      const [readiness, providers, generations, persistence, epochs] = await Promise.all([
        getReadiness(),
        listProviderHealth(15),
        listRuntimeGenerations(generationCursor),
        usageCompleteness({ start: start.toISOString(), end: end.toISOString() }),
        listUsageGatewayEpochs('unresolved', epochCursor)
      ]);
      return { readiness, providers, generations, persistence, epochs };
    },
    refetchInterval: 15_000
  }));

  function healthTone(value: string) {
    if (['healthy', 'ok', 'active', 'passing'].includes(value.toLowerCase())) return 'success';
    if (['degraded', 'stale', 'unknown', 'unavailable_lkg'].includes(value.toLowerCase())) return 'warning';
    return 'danger';
  }

  function percent(success: number, total: number) {
    return total === 0 ? 'No traffic' : `${((success / total) * 100).toFixed(1)}%`;
  }

  function nextGenerationPage() {
    const next = health.data?.generations.next_cursor ?? undefined;
    if (!next) return;
    generationHistory = [...generationHistory, generationCursor];
    generationCursor = next;
  }

  function previousGenerationPage() {
    generationCursor = generationHistory.at(-1);
    generationHistory = generationHistory.slice(0, -1);
  }

  function nextEpochPage() {
    const next = health.data?.epochs.next_cursor ?? undefined;
    if (!next) return;
    epochHistory = [...epochHistory, epochCursor];
    epochCursor = next;
  }

  function previousEpochPage() {
    epochCursor = epochHistory.at(-1);
    epochHistory = epochHistory.slice(0, -1);
  }

  async function acknowledgeEpoch(processEpoch: string, gateway: string) {
    if (!window.confirm(`Acknowledge the investigated unclean epoch for ${gateway}? Retained gap evidence will not be removed.`)) return;
    busyEpoch = processEpoch;
    epochError = '';
    epochNotice = '';
    try {
      await acknowledgeUsageGatewayEpoch(processEpoch);
      epochNotice = `Epoch ${processEpoch} acknowledged. Historical completeness evidence remains retained.`;
      await health.refetch();
    } catch (error) {
      epochError = error instanceof Error ? error.message : 'The epoch could not be acknowledged.';
    } finally {
      busyEpoch = '';
    }
  }
</script>

<svelte:head><title>Health · OpenLLMProxy</title></svelte:head>

<div class="page-header">
  <div><p class="eyebrow">Operations</p><h1 class="page-title">Health</h1><p class="page-description">Gateway dependencies, provider outcomes, runtime convergence, and persistence completeness.</p></div>
  <button class="button button-secondary" type="button" onclick={() => health.refetch()} disabled={health.isFetching}>Refresh</button>
</div>

<p class="refresh-note" aria-live="polite">Automatically refreshes every 15 seconds{health.dataUpdatedAt ? ` · Last checked ${new Date(health.dataUpdatedAt).toLocaleTimeString()}` : ''}.</p>

{#if health.isPending}
  <div class="loading-state" role="status">Checking the installation…</div>
{:else if health.isError}
  <div class="inline-problem" role="alert"><strong>Control health is unavailable.</strong> The gateway may still be serving its last-known-good runtime. <button class="text-button" onclick={() => health.refetch()}>Try again</button></div>
{:else if health.data}
  <section class="metric-grid" aria-label="Dependency readiness">
    <article class="card metric-card"><p>Gateway</p><strong><span class="badge {healthTone(health.data.readiness.status)}">{health.data.readiness.status}</span></strong></article>
    <article class="card metric-card"><p>PostgreSQL</p><strong><span class="badge {healthTone(health.data.readiness.database)}">{health.data.readiness.database.replaceAll('_', ' ')}</span></strong></article>
    <article class="card metric-card"><p>Distributed limits</p><strong><span class="badge {healthTone(health.data.readiness.limits)}">{health.data.readiness.limits.replaceAll('_', ' ')}</span></strong></article>
    <article class="card metric-card"><p>Active generation</p><strong>#{health.data.readiness.generation ?? '—'}</strong></article>
  </section>

  <section class="card persistence" aria-labelledby="persistence-title">
    <div class="health-icon" class:ok={health.data.persistence.complete} aria-hidden="true">{health.data.persistence.complete ? '✓' : '!'}</div>
    <div><p class="eyebrow">Last 24 hours</p><h2 id="persistence-title">{health.data.persistence.complete ? 'Usage persistence is complete' : 'Usage persistence needs attention'}</h2><p>{health.data.persistence.ingestion_gap_events} gap-event lower bound · {health.data.persistence.uncertain_gap_count} uncertain gateway epochs · {health.data.persistence.incomplete_count} incomplete requests · {health.data.persistence.unpriced_count} unpriced requests. Missing or uncertain metadata is reported, never silently converted to zero cost.</p></div>
  </section>

  <section class="section" aria-labelledby="epochs-title">
    <div class="section-heading"><div><p class="eyebrow">Usage durability</p><h2 id="epochs-title">Unresolved gateway epochs</h2><p class="section-description">An unclean process epoch keeps readiness degraded until an operator investigates and acknowledges it. Acknowledgement is audited and never deletes its retained loss or uncertainty evidence.</p></div><span class:warning={health.data.epochs.data.length > 0} class:success={health.data.epochs.data.length === 0} class="badge">{health.data.epochs.data.length} on page</span></div>
    {#if epochNotice}<div class="inline-notice" role="status">{epochNotice}</div>{/if}
    {#if epochError}<div class="inline-problem" role="alert">{epochError}</div>{/if}
    {#if health.data.epochs.data.length === 0 && epochHistory.length === 0}
      <div class="card empty-state">No unclean gateway epoch awaits acknowledgement.</div>
    {:else}
      <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
      <div class="table-shell" tabindex="0" role="region" aria-label="Unresolved usage gateway epochs"><table class="data-table"><caption class="sr-only">Unclean gateway process epochs awaiting operator acknowledgement</caption><thead><tr><th scope="col">Gateway</th><th scope="col">Detected</th><th scope="col">Accepted / persisted</th><th scope="col">Dropped / abandoned</th><th scope="col">Uncertain lower bound</th><th scope="col"><span class="sr-only">Action</span></th></tr></thead><tbody>{#each health.data.epochs.data as epoch (epoch.process_epoch)}<tr><td><strong>{epoch.gateway_instance}</strong><br /><code>{epoch.process_epoch}</code></td><td>{formatDate(epoch.stale_detected_at ?? epoch.updated_at)}</td><td>{epoch.accepted} / {epoch.persisted}</td><td>{epoch.dropped} / {epoch.abandoned}</td><td>{epoch.uncertain_event_lower_bound}</td><td><button class="button button-secondary" type="button" onclick={() => acknowledgeEpoch(epoch.process_epoch, epoch.gateway_instance)} disabled={Boolean(busyEpoch)}>{busyEpoch === epoch.process_epoch ? 'Acknowledging…' : 'Acknowledge epoch'}</button></td></tr>{/each}</tbody></table></div>
      <CursorPagination page={epochHistory.length + 1} hasPrevious={epochHistory.length > 0} hasNext={Boolean(health.data.epochs.next_cursor)} onPrevious={previousEpochPage} onNext={nextEpochPage} label="Unresolved gateway epoch pages" />
    {/if}
  </section>

  <section class="section" aria-labelledby="providers-title">
    <div class="section-heading"><div><p class="eyebrow">Rolling 15 minutes</p><h2 id="providers-title">Providers</h2></div><span class="badge">{health.data.providers.data.length} configured</span></div>
    {#if health.data.providers.data.length === 0}
      <div class="card empty-state">No providers are configured.</div>
    {:else}
      <div class="provider-grid">
        {#each health.data.providers.data as provider (provider.provider_id)}
          <article class="card provider-card">
            <div class="provider-heading"><div><h3>{provider.provider_name}</h3><p>{provider.provider_kind} · {provider.provider_state}</p></div><span class="badge {healthTone(provider.status)}">{provider.status}</span></div>
            <dl><div><dt>Success rate</dt><dd>{percent(provider.success_count, provider.attempt_count)}</dd></div><div><dt>Average latency</dt><dd>{provider.average_latency_ms == null ? '—' : `${provider.average_latency_ms.toFixed(0)} ms`}</dd></div><div><dt>Rate limited</dt><dd>{provider.rate_limit_count}</dd></div><div><dt>5xx / transport</dt><dd>{provider.server_error_count} / {provider.transport_error_count}</dd></div></dl>
            <p class="probe"><strong>Last probe:</strong> {provider.last_probe_detail ?? provider.last_probe_status ?? 'Not probed'}<br /><span>{formatDate(provider.last_probe_at)}</span></p>
          </article>
        {/each}
      </div>
    {/if}
  </section>

  <section class="section" aria-labelledby="runtime-title">
    <div class="section-heading"><div><p class="eyebrow">Configuration</p><h2 id="runtime-title">Runtime generations</h2></div></div>
    <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
    <div class="table-shell" tabindex="0" role="region" aria-label="Runtime generation history"><table class="data-table"><caption class="sr-only">Recently published immutable runtime generations</caption><thead><tr><th scope="col">Generation</th><th scope="col">Digest</th><th scope="col">Activated by</th><th scope="col">Created</th><th scope="col">Gateway state</th></tr></thead><tbody>{#each health.data.generations.data as generation (generation.id)}<tr><td><strong>#{generation.sequence}</strong></td><td class="mono">{generation.sha256.slice(0, 16)}…</td><td>{generation.created_by_email}</td><td>{formatDate(generation.created_at)}</td><td>{#if generation.sequence === health.data.readiness.generation}<span class="badge success">Loaded</span>{:else}<span class="badge">Historical</span>{/if}</td></tr>{/each}</tbody></table></div>
    <CursorPagination page={generationHistory.length + 1} hasPrevious={generationHistory.length > 0} hasNext={Boolean(health.data.generations.next_cursor)} onPrevious={previousGenerationPage} onNext={nextGenerationPage} label="Runtime generation pages" />
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
  code { font: .72rem 'JetBrains Mono Variable', monospace; overflow-wrap: anywhere; }
  .section-heading, .provider-heading { display: flex; align-items: flex-start; justify-content: space-between; gap: 1rem; margin-bottom: 0.75rem; }
  .provider-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 0.85rem; }
  .provider-card { padding: 1rem; }
  .provider-heading p { margin: 0.15rem 0 0; color: var(--foreground-muted); font-size: 0.75rem; }
  dl { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 0.75rem; margin: 1rem 0 0; }
  dt { color: var(--foreground-muted); font-size: 0.7rem; font-weight: 700; }
  dd { margin: 0.1rem 0 0; font-weight: 700; }
  .probe { margin: 1rem 0 0; padding-top: 0.8rem; border-top: 1px solid var(--border); color: var(--foreground-muted); font-size: 0.75rem; overflow-wrap: anywhere; }
  @media (max-width: 60rem) { .provider-grid { grid-template-columns: 1fr; } }
  @media (max-width: 36rem) { .persistence { display: grid; } dl { grid-template-columns: 1fr; } }
</style>
