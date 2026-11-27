<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import QueryView from '#lib/components/QueryView.svelte';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import PageHeader from '#lib/components/PageHeader.svelte';
  import { healthKeys } from '#lib/features/health/healthKeys.js';

  import { createQuery } from '@tanstack/svelte-query';
  import CursorPagination from '#lib/components/CursorPagination.svelte';
  import {
    cursorPaginationProps,
    emptyCursorHistory
  } from '#lib/listing/pagination.js';
  import {
    getReadiness,
    listProviderHealth,
    listRequestMetadataGatewayEpochs
  } from '#lib/features/health/api.js';
  import { errorMessage } from '#lib/management/http.js';
  import { listRuntimeGenerations } from '#lib/features/health/runtime-generations/api.js';
  import { usageCompleteness } from '#lib/features/usage/index.js';
  import { formatDate, formatTime } from '#lib/format.js';
  import GatewayEpochsPanel from '#lib/features/health/GatewayEpochsPanel.svelte';
  import ProviderHealthPanel from '#lib/features/health/ProviderHealthPanel.svelte';
  import ReadinessPanels from '#lib/features/health/ReadinessPanels.svelte';
  import { useRole } from '#lib/session/useRole.svelte.js';

  // Runtime generations and unresolved gateway epochs are installation-wide
  // reads, so they run only for principals the contract admits.
  const access = useRole();
  const epochsAllowed = $derived(
    access.allows('GET /api/v1/request-metadata/gateway-epochs')
  );
  const generationPagination = $state(emptyCursorHistory());
  const epochPagination = $state(emptyCursorHistory());
  const refetchInterval = 15_000;
  let windowMinutes = $state(15);

  const readiness = createQuery(() => ({
    queryKey: healthKeys.readiness(),
    queryFn: () => getReadiness(),
    refetchInterval
  }));
  const providers = createQuery(() => ({
    queryKey: healthKeys.providers(windowMinutes),
    queryFn: () => listProviderHealth(windowMinutes),
    placeholderData: (previous) => previous,
    refetchInterval
  }));
  const persistence = createQuery(() => ({
    queryKey: healthKeys.persistence(),
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
    queryKey: healthKeys.generations(generationPagination.cursor),
    queryFn: () => listRuntimeGenerations(generationPagination.cursor),
    placeholderData: (previous) => previous,
    enabled: access.globalScope,
    refetchInterval
  }));
  const epochs = createQuery(() => ({
    queryKey: healthKeys.epochs(epochPagination.cursor),
    queryFn: () =>
      listRequestMetadataGatewayEpochs('unresolved', epochPagination.cursor),
    placeholderData: (previous) => previous,
    enabled: epochsAllowed,
    refetchInterval
  }));

  const panels = $derived([
    readiness,
    providers,
    persistence,
    ...(access.globalScope ? [generations] : []),
    ...(epochsAllowed ? [epochs] : [])
  ]);
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

<PageTitle title="Health" />

<PageHeader
  title="Health"
  description="Gateway dependencies, worker checkpoints, provider outcomes, runtime convergence, and persistence completeness."
>
  {#snippet actions()}
    <button
      class="button button-secondary"
      type="button"
      onclick={refresh}
      disabled={fetching}
    >
      Refresh
    </button>
  {/snippet}
</PageHeader>
<p class="refresh-note" aria-live="polite">
  Automatically refreshes every 15 seconds{checkedAt
    ? ` · Last checked ${formatTime(checkedAt)}`
    : ''}.{fetching ? ' Checking now…' : ''}
</p>

{#if readiness.isError}
  <div class="health-problem">
    <Callout tone="danger" title="Control health is unavailable.">
      {errorMessage(readiness.error, 'The control API did not respond.')} The gateway
      may still be serving its last-known-good runtime.
      {#snippet actions()}<button
          class="button button-secondary"
          type="button"
          onclick={() => readiness.refetch()}
        >
          Try again
        </button>{/snippet}
    </Callout>
  </div>
{:else if !readiness.data}
  <div class="loading-state" role="status">Checking the installation…</div>
{:else}
  <ReadinessPanels
    readiness={readiness.data}
    observedAt={readiness.dataUpdatedAt}
    {persistence}
  />

  {#if epochsAllowed}
    <GatewayEpochsPanel {epochs} pagination={epochPagination} {readiness} />
  {/if}

  <ProviderHealthPanel {providers} bind:windowMinutes />
{/if}

{#if readiness.data && access.globalScope}
  <section class="section" aria-labelledby="runtime-title">
    <div class="section-heading">
      <div>
        <p class="eyebrow">Configuration</p>
        <h2 id="runtime-title">Runtime generations</h2>
      </div>
    </div>
    <QueryView
      query={generations}
      compact
      loading="Loading runtime generations…"
      failure={errorMessage(
        generations.error,
        'Runtime generations are unavailable.'
      )}
      retryLabel="Try again"
      updating={generations.isPlaceholderData}
    >
      <ScrollTable
        label="Runtime generation history"
        busy={generations.isPlaceholderData}
      >
        <table class="data-table">
          <caption class="sr-only">
            Recently published immutable runtime generations
          </caption><thead>
            <tr>
              <th scope="col">Generation</th><th scope="col">Digest</th><th
                scope="col"
              >
                Activated by
              </th><th scope="col">Created</th><th scope="col">
                Gateway state
              </th>
            </tr>
          </thead><tbody>
            {#each generations.data?.items ?? [] as generation (generation.id)}<tr>
                <td><strong>#{generation.sequence}</strong></td><td
                  class="mono"
                >
                  {generation.sha256.slice(0, 16)}…
                </td><td>{generation.created_by_email}</td><td>
                  {formatDate(generation.created_at)}
                </td><td>
                  {#if generation.sequence === readiness.data.generation}<span
                      class="badge success"
                    >
                      Loaded
                    </span>{:else}<span class="badge">Historical</span>{/if}
                </td>
              </tr>{/each}
          </tbody>
        </table>
      </ScrollTable>
      <CursorPagination
        {...cursorPaginationProps(
          generationPagination,
          generations.isPlaceholderData ? null : generations.data?.nextCursor
        )}
        label="Runtime generation pages"
      />
    </QueryView>
  </section>
{/if}

<style>
  .health-problem {
    margin-top: 1rem;
  }
  .refresh-note {
    margin: 1rem 0 0;
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
</style>
