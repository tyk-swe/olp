<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import {
    cursorPaginationProps,
    emptyCursorHistory
  } from '$lib/api/pagination';
  import {
    getReadiness,
    listProviderHealth,
    listRequestMetadataGatewayEpochs
  } from '$lib/api/health';
  import { errorMessage } from '$lib/api/http';
  import { listRuntimeGenerations } from '$lib/api/runtime';
  import { usageCompleteness } from '$lib/api/usage';
  import { formatDate } from '$lib/format';
  import GatewayEpochsPanel from './GatewayEpochsPanel.svelte';
  import ProviderHealthPanel from './ProviderHealthPanel.svelte';
  import ReadinessPanels from './ReadinessPanels.svelte';

  const generationPagination = $state(emptyCursorHistory());
  const epochPagination = $state(emptyCursorHistory());
  const refetchInterval = 15_000;
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
      return usageCompleteness({
        start: start.toISOString(),
        end: end.toISOString()
      });
    },
    refetchInterval
  }));
  const generations = createQuery(() => ({
    queryKey: [
      'operator-health',
      'generations',
      generationPagination.cursor ?? 'first'
    ],
    queryFn: () => listRuntimeGenerations(generationPagination.cursor),
    placeholderData: (previous) => previous,
    refetchInterval
  }));
  const epochs = createQuery(() => ({
    queryKey: ['operator-health', 'epochs', epochPagination.cursor ?? 'first'],
    queryFn: () =>
      listRequestMetadataGatewayEpochs('unresolved', epochPagination.cursor),
    placeholderData: (previous) => previous,
    refetchInterval
  }));

  const panels = [readiness, providers, persistence, generations, epochs];
  const fetching = $derived(panels.some((panel) => panel.isFetching));
  const checkedAt = $derived.by(() => {
    const stamps = panels
      .map((panel) => panel.dataUpdatedAt)
      .filter((at) => at > 0);
    return stamps.length === 0 ? 0 : Math.min(...stamps);
  });

  function refresh() {
    for (const panel of panels) void panel.refetch();
  }
</script>

<svelte:head><title>Health · OpenLLMProxy</title></svelte:head>

<div class="page-header">
  <div>
    <p class="eyebrow">Operations</p>
    <h1 class="page-title">Health</h1>
    <p class="page-description">
      Gateway dependencies, worker checkpoints, provider outcomes, runtime
      convergence, and persistence completeness.
    </p>
  </div>
  <button
    class="button button-secondary"
    type="button"
    onclick={refresh}
    disabled={fetching}>Refresh</button
  >
</div>

<p class="refresh-note" aria-live="polite">
  Automatically refreshes every 15 seconds{checkedAt
    ? ` · Last checked ${new Date(checkedAt).toLocaleTimeString()}`
    : ''}.{fetching ? ' Checking now…' : ''}
</p>

{#if readiness.isError}
  <div class="inline-problem" role="alert">
    <strong>Control health is unavailable.</strong>
    {errorMessage(readiness.error, 'The control API did not respond.')} The gateway
    may still be serving its last-known-good runtime.
    <button class="text-button" onclick={() => readiness.refetch()}
      >Try again</button
    >
  </div>
{:else if !readiness.data}
  <div class="loading-state" role="status">Checking the installation…</div>
{:else}
  <ReadinessPanels
    readiness={readiness.data}
    observedAt={readiness.dataUpdatedAt}
    {persistence}
  />

  <GatewayEpochsPanel {epochs} pagination={epochPagination} {readiness} />

  <ProviderHealthPanel {providers} bind:windowMinutes />

  <section class="section" aria-labelledby="runtime-title">
    <div class="section-heading">
      <div>
        <p class="eyebrow">Configuration</p>
        <h2 id="runtime-title">Runtime generations</h2>
      </div>
    </div>
    {#if generations.isError}
      <div class="inline-problem" role="alert">
        {errorMessage(
          generations.error,
          'Runtime generations are unavailable.'
        )}
        <button class="text-button" onclick={() => generations.refetch()}
          >Try again</button
        >
      </div>
    {:else if !generations.data}
      <div class="loading-state" role="status">
        Loading runtime generations…
      </div>
    {:else}
      <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
      <div
        class="table-shell"
        tabindex="0"
        role="region"
        aria-label="Runtime generation history"
      >
        <table class="data-table">
          <caption class="sr-only"
            >Recently published immutable runtime generations</caption
          ><thead
            ><tr
              ><th scope="col">Generation</th><th scope="col">Digest</th><th
                scope="col">Activated by</th
              ><th scope="col">Created</th><th scope="col">Gateway state</th
              ></tr
            ></thead
          ><tbody
            >{#each generations.data.items as generation (generation.id)}<tr
                ><td><strong>#{generation.sequence}</strong></td><td
                  class="mono">{generation.sha256.slice(0, 16)}…</td
                ><td>{generation.created_by_email}</td><td
                  >{formatDate(generation.created_at)}</td
                ><td
                  >{#if generation.sequence === readiness.data.generation}<span
                      class="badge success">Loaded</span
                    >{:else}<span class="badge">Historical</span>{/if}</td
                ></tr
              >{/each}</tbody
          >
        </table>
      </div>
      <CursorPagination
        {...cursorPaginationProps(
          generationPagination,
          generations.isPlaceholderData ? null : generations.data.nextCursor
        )}
        label="Runtime generation pages"
      />
    {/if}
  </section>
{/if}

<style>
  .refresh-note {
    margin: 1rem 0 0;
    color: var(--foreground-muted);
    font-size: 0.75rem;
  }
</style>
