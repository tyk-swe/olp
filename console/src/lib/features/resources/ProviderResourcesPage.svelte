<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import {
    listProviderResources,
    type ProviderResourceFilters
  } from '$lib/features/resources/api';
  import { errorMessage } from '$lib/api/http';
  import {
    cursorPaginationProps,
    emptyCursorHistory
  } from '$lib/lists/pagination';
  import { formatDate } from '$lib/format';

  const kinds: ProviderResourceFilters['kind'][] = [
    'file',
    'batch',
    'response'
  ];

  let kind = $state('');
  let route = $state('');
  let resourceState = $state('');
  let apiKeyId = $state('');
  let providerId = $state('');

  let applied = $state<Omit<ProviderResourceFilters, 'cursor'>>({ limit: 50 });
  let paging = $state(emptyCursorHistory());

  const resources = createQuery(() => ({
    queryKey: ['provider-resources', applied, paging.cursor] as const,
    queryFn: () => listProviderResources({ ...applied, cursor: paging.cursor }),
    placeholderData: (previous) => previous
  }));

  function apply(event: SubmitEvent) {
    event.preventDefault();
    applied = {
      limit: 50,
      kind: (kind || undefined) as ProviderResourceFilters['kind'],
      route: route.trim() || undefined,
      state: resourceState.trim() || undefined,
      api_key_id: apiKeyId.trim() || undefined,
      provider_id: providerId.trim() || undefined
    };
    paging.cursor = undefined;
    paging.history = [];
  }

  function clear() {
    kind = '';
    route = '';
    resourceState = '';
    apiKeyId = '';
    providerId = '';
    applied = { limit: 50 };
    paging.cursor = undefined;
    paging.history = [];
  }
</script>

<svelte:head><title>Provider Resources · OpenLLMProxy</title></svelte:head>

<div class="page-header">
  <div>
    <p class="eyebrow">Operations</p>
    <h1 class="page-title">Provider Resources</h1>
    <p class="page-description">
      Metadata-only mappings for files, batches, and stored responses. Provider
      content never appears in the console.
    </p>
  </div>
  <button
    class="button button-secondary"
    type="button"
    onclick={() => resources.refetch()}
    disabled={resources.isFetching}>Refresh</button
  >
</div>
<form
  class="card filters"
  aria-label="Provider resource filters"
  onsubmit={apply}
>
  <label
    >Kind <select bind:value={kind}
      ><option value="">All kinds</option>{#each kinds as value (value)}<option
          {value}>{value}</option
        >{/each}</select
    ></label
  >
  <label>Route <input bind:value={route} placeholder="All routes" /></label>
  <label
    >State <input bind:value={resourceState} placeholder="All states" /></label
  >
  <label
    >API key ID <input
      bind:value={apiKeyId}
      class="mono"
      placeholder="All keys"
    /></label
  >
  <label
    >Provider ID <input
      bind:value={providerId}
      class="mono"
      placeholder="All providers"
    /></label
  >
  <div class="filter-actions">
    <button class="button button-primary" type="submit">Apply filters</button
    ><button class="button button-secondary" type="button" onclick={clear}
      >Clear</button
    >
  </div>
</form>
{#if resources.isPending}<div class="loading-state" role="status">
    Loading provider resources…
  </div>
{:else if resources.isError}<div class="inline-problem" role="alert">
    {errorMessage(resources.error, 'Provider resources are unavailable.')}
    <button
      class="text-button"
      type="button"
      onclick={() => resources.refetch()}>Retry</button
    >
  </div>
{:else if resources.data?.items.length === 0}<section class="card empty-state">
    <p>No provider resources match these filters.</p>
  </section>
{:else}
  <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
  <div
    class="table-shell"
    tabindex="0"
    role="region"
    aria-label="Provider resource results"
  >
    <table class="data-table">
      <caption class="sr-only">Provider resource mappings</caption><thead
        ><tr
          ><th scope="col">ID</th><th scope="col">Kind</th><th scope="col"
            >Route</th
          ><th scope="col">Provider</th><th scope="col">API key</th><th
            scope="col">State</th
          ><th scope="col">Expires</th><th scope="col">Updated</th></tr
        ></thead
      ><tbody
        >{#each resources.data?.items ?? [] as item (item.id)}<tr
            ><td data-label="ID"><code>{item.id}</code></td><td
              data-label="Kind">{item.kind}</td
            ><td data-label="Route">{item.route}</td><td data-label="Provider"
              >{item.provider_name}<small>{item.upstream_model}</small></td
            ><td data-label="API key"
              >{item.api_key_name}<small class="mono">{item.api_key_id}</small
              ></td
            ><td data-label="State">{item.state}</td><td data-label="Expires"
              >{item.expires_at ? formatDate(item.expires_at) : '—'}</td
            ><td data-label="Updated">{formatDate(item.updated_at)}</td></tr
          >{/each}</tbody
      >
    </table>
  </div>
  <CursorPagination
    {...cursorPaginationProps(
      paging,
      resources.isPlaceholderData ? null : resources.data?.nextCursor
    )}
    label="Provider resource pages"
  />
{/if}

<style>
  .filters {
    display: flex;
    flex-wrap: wrap;
    align-items: end;
    gap: 0.75rem;
    margin: 1.5rem 0;
    padding: 1.25rem;
  }
  .filters label {
    display: grid;
    gap: 0.4rem;
    font-size: var(--text-body-sm);
    font-weight: 500;
  }
  .filters input,
  .filters select {
    min-height: 2.5rem;
    padding: 0.5rem 0.75rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--foreground);
    font-size: var(--text-body-sm);
    font-weight: 400;
    transition: border-color var(--motion);
  }
  .filters input:hover,
  .filters select:hover {
    border-color: var(--border-strong);
  }
  .filter-actions {
    display: flex;
    gap: 0.5rem;
  }
  td small {
    display: block;
    color: var(--foreground-muted);
  }
  td code {
    overflow-wrap: anywhere;
    font-family: var(--font-mono);
    font-size: var(--text-caption);
  }
  .table-shell {
    overflow-x: auto;
  }
</style>
