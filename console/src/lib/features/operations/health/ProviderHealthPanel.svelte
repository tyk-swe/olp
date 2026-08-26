<script lang="ts">
  import type { CreateQueryResult } from '@tanstack/svelte-query';
  import type { ProviderHealth } from '$lib/api/health';
  import { errorMessage } from '$lib/api/http';
  import { formatDate } from '$lib/format';
  import { healthTone } from './presentation';

  let {
    providers,
    windowMinutes = $bindable()
  }: {
    providers: CreateQueryResult<{
      window_minutes: number;
      data: ProviderHealth[];
    }>;
    windowMinutes: number;
  } = $props();

  // The backend accepts 1 through 1440 minutes; these are the operator-facing
  // windows worth one click.
  const windowOptions = [
    { minutes: 5, label: '5 minutes' },
    { minutes: 15, label: '15 minutes' },
    { minutes: 60, label: '1 hour' },
    { minutes: 1440, label: '24 hours' }
  ];

  function percent(success: number, total: number) {
    return total === 0
      ? 'No traffic'
      : `${((success / total) * 100).toFixed(1)}%`;
  }
</script>

<section class="section" aria-labelledby="providers-title">
  <div class="section-heading">
    <div>
      <p class="eyebrow">Rolling window</p>
      <h2 id="providers-title">Providers</h2>
    </div>
    <div class="heading-controls">
      <label class="window-select" for="provider-window"
        >Window <select id="provider-window" bind:value={windowMinutes}
          >{#each windowOptions as option (option.minutes)}<option
              value={option.minutes}>{option.label}</option
            >{/each}</select
        ></label
      >{#if providers.data}<span class="badge"
          >{providers.data.data.length} configured</span
        >{/if}
    </div>
  </div>
  {#if providers.isError}
    <div class="inline-problem" role="alert">
      {errorMessage(providers.error, 'Provider outcomes are unavailable.')}
      <button class="text-button" onclick={() => providers.refetch()}
        >Try again</button
      >
    </div>
  {:else if !providers.data}
    <div class="loading-state" role="status">Loading provider outcomes…</div>
  {:else if providers.data.data.length === 0}
    <div class="card empty-state">No providers are configured.</div>
  {:else}
    <div class="provider-grid">
      {#each providers.data.data as provider (provider.provider_id)}
        <article class="card provider-card">
          <div class="provider-heading">
            <div>
              <h3>{provider.provider_name}</h3>
              <p>{provider.provider_kind} · {provider.provider_state}</p>
            </div>
            <span class="badge {healthTone(provider.status)}"
              >{provider.status}</span
            >
          </div>
          <dl>
            <div>
              <dt>Success rate</dt>
              <dd>
                {percent(provider.success_count, provider.attempt_count)}
              </dd>
            </div>
            <div>
              <dt>Average latency</dt>
              <dd>
                {provider.average_latency_ms == null
                  ? '—'
                  : `${provider.average_latency_ms.toFixed(0)} ms`}
              </dd>
            </div>
            <div>
              <dt>Rate limited</dt>
              <dd>{provider.rate_limit_count}</dd>
            </div>
            <div>
              <dt>5xx / transport</dt>
              <dd>
                {provider.server_error_count} / {provider.transport_error_count}
              </dd>
            </div>
          </dl>
          <p class="probe">
            <strong>Last probe:</strong>
            {provider.last_probe_detail ??
              provider.last_probe_status ??
              'Not probed'}<br /><span
              >{formatDate(provider.last_probe_at)}</span
            ><br /><strong>Last live attempt:</strong>
            <span
              >{provider.last_attempt_at
                ? formatDate(provider.last_attempt_at)
                : 'No traffic in this window'}</span
            >
          </p>
        </article>
      {/each}
    </div>
    <p class="section-link">
      Counted over the last {providers.data.window_minutes}
      {providers.data.window_minutes === 1 ? 'minute' : 'minutes'}. Provider
      probe failures stay separate from gateway admission failures.
    </p>
  {/if}
</section>

<style>
  .text-button {
    min-height: 2.75rem;
    border: 0;
    background: transparent;
    color: var(--accent-strong);
    font-weight: 700;
  }
  h2,
  h3 {
    margin: 0;
    letter-spacing: -0.025em;
  }
  h2 {
    font-size: 1.2rem;
  }
  h3 {
    font-size: 1rem;
  }
  .section {
    margin-top: 2rem;
  }
  .section-link {
    margin: 0.6rem 0 0;
    color: var(--foreground-muted);
    font-size: 0.78rem;
  }
  .section-heading,
  .provider-heading {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 1rem;
    margin-bottom: 0.75rem;
  }
  .heading-controls {
    display: flex;
    flex: none;
    align-items: center;
    gap: 0.6rem;
  }
  .window-select {
    display: grid;
    gap: 0.25rem;
    color: var(--foreground-muted);
    font-size: 0.7rem;
    font-weight: 700;
  }
  /* The label is deliberately small and bold; the chosen window is content and
     keeps a readable control size. */
  .window-select select {
    min-height: 2.5rem;
    padding: 0.35rem 0.6rem;
    border: 1px solid var(--border-strong);
    border-radius: 0.375rem;
    background: var(--surface);
    color: var(--foreground);
    font-size: 0.8125rem;
    font-weight: 600;
  }
  .provider-grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 0.85rem;
  }
  .provider-card {
    padding: 1rem;
  }
  .provider-heading p {
    margin: 0.15rem 0 0;
    color: var(--foreground-muted);
    font-size: 0.75rem;
  }
  dl {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 0.75rem;
    margin: 1rem 0 0;
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
  .probe {
    margin: 1rem 0 0;
    padding-top: 0.8rem;
    border-top: 1px solid var(--border);
    color: var(--foreground-muted);
    font-size: 0.75rem;
    overflow-wrap: anywhere;
  }
  @media (max-width: 60rem) {
    .provider-grid {
      grid-template-columns: 1fr;
    }
  }
  @media (max-width: 36rem) {
    dl {
      grid-template-columns: 1fr;
    }
    .section-heading {
      display: grid;
    }
  }
</style>
