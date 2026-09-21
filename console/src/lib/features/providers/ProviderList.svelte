<script lang="ts">
  import ProviderBulkActions from './ProviderBulkActions.svelte';
  import { providerKeys } from '$lib/features/providers/providerKeys';

  import { overviewKeys } from '$lib/features/overview/overviewKeys';

  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage as message } from '$lib/api/http';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import { listProviderPage } from '$lib/features/providers/api';
  import { cursorPaginationProps, resetCursor } from '$lib/lists/pagination';
  import { debouncedSearch } from '$lib/lists/search.svelte';
  import type { ProviderListState } from './providerPagination';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { formatDate, stateLabel } from '$lib/format';
  import {
    providerStatus,
    providerStatusTone
  } from '$lib/features/providers/providerEditor';

  let { listState = $bindable() }: { listState: ProviderListState } = $props();
  const access = useRole();
  const canManage = $derived(access.can('providers.manage'));
  const queryClient = useQueryClient();
  let selectedIds = $state<string[]>([]);
  const selectedSet = $derived(new Set(selectedIds));

  function applySearch() {
    if (listState.search === listState.applied) return;
    listState.applied = listState.search;
    resetCursor(listState);
    selectedIds = [];
  }
  const search = debouncedSearch(applySearch);
  // A detail visit during the debounce window leaves typed text unapplied;
  // resume the pause on return so the input the operator sees is the search
  // that runs instead of being dropped or applied eagerly.
  if (listState.search !== listState.applied) search.schedule();

  const providers = createQuery(() => {
    const cursor = listState.cursor;
    const applied = listState.applied;
    return {
      queryKey: [...providerKeys.page(cursor), applied],
      queryFn: ({ signal }) => listProviderPage(cursor, signal, applied),
      placeholderData: (previous) => previous
    };
  });
  const groups = $derived(
    [
      ...Map.groupBy(
        providers.data?.items ?? [],
        (provider) => provider.vendor_id ?? 'Custom'
      ).entries()
    ].sort(([a], [b]) => a.localeCompare(b))
  );
  const selected = $derived(
    (providers.data?.items ?? []).filter((provider) =>
      selectedSet.has(provider.id)
    )
  );
</script>

<div class="page-header">
  <div>
    <p class="eyebrow">Gateway</p>
    <h1 class="page-title">Providers</h1>
    <p class="page-description">
      Connect upstream providers, review their models, and manage activation.
    </p>
  </div>
  {#if canManage}<a
      class="button button-primary"
      href={resolve('/providers/new')}>Add provider <NavIcon name="arrow" /></a
    >{/if}
</div>
{#if !canManage}
  <ReadOnlyNote>
    Your role can view providers but not connect, edit, or activate them.
  </ReadOnlyNote>
{/if}

<label for="provider-search">Search connections</label><input
  id="provider-search"
  bind:value={listState.search}
  oninput={() => {
    if (listState.search === '') search.applyNow();
    else search.schedule();
  }}
  placeholder="Name or vendor"
  type="search"
/>
{#if canManage}<ProviderBulkActions
    {selected}
    onChanged={async () => {
      await providers.refetch();
      await queryClient.invalidateQueries({ queryKey: overviewKeys.root });
      selectedIds = [];
    }}
  />{/if}

{#if providers.isPending}
  <div class="loading-state" role="status">Loading providers…</div>
{:else if providers.isError}
  <div class="inline-problem" role="alert">
    {message(providers.error)}
    <button
      class="button button-secondary"
      type="button"
      onclick={() => providers.refetch()}>Retry</button
    >
  </div>
{:else if providers.data?.items.length === 0 && listState.history.length === 0}
  <section class="card empty-state">
    <div>
      <h2>No providers configured</h2>
      <p>Connect an upstream and test it before building a route.</p>
      {#if canManage}<a
          class="button button-primary"
          href={resolve('/providers/new')}>Connect provider</a
        >{/if}
    </div>
  </section>
{:else}
  {#if providers.isPlaceholderData}
    <p class="updating" role="status">Updating…</p>
  {/if}
  <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
  <div
    class="table-shell provider-table"
    tabindex="0"
    role="region"
    aria-label="Providers"
    aria-busy={providers.isPlaceholderData}
  >
    <table class="data-table">
      <thead
        ><tr
          ><th><span class="sr-only">Select</span></th><th>Name</th><th
            >Vendor / connector</th
          ><th>Status</th><th>Models</th><th>Last probe</th><th
            ><span class="sr-only">Actions</span></th
          ></tr
        ></thead
      ><tbody
        >{#each groups as [vendor, connections] (vendor)}<tr
            class="vendor-group"
            ><th colspan="7" scope="rowgroup"
              >{vendor} · {connections.length} connections on this page</th
            ></tr
          >{#each connections as item (item.id)}<tr
              ><td
                >{#if canManage}<input
                    type="checkbox"
                    aria-label="Select {item.name}"
                    checked={selectedSet.has(item.id)}
                    disabled={providers.isPlaceholderData}
                    onchange={(e) => {
                      selectedIds = e.currentTarget.checked
                        ? [...selectedIds, item.id]
                        : selectedIds.filter((id) => id !== item.id);
                    }}
                  />{/if}</td
              ><td
                ><a class="table-link" href={resolve(`/providers/${item.id}`)}
                  >{item.name}</a
                ><br /><small>{item.project_name ?? 'Installation-wide'}</small
                ></td
              ><td
                >{item.vendor_id ?? 'Custom'}<small class="connector-name"
                  >{stateLabel(item.kind)}</small
                ></td
              ><td
                ><span class="badge {providerStatusTone(item)}"
                  >{providerStatus(item)}</span
                ></td
              ><td>{item.enabled_model_count} enabled</td><td
                >{item.last_probe_at
                  ? formatDate(item.last_probe_at)
                  : 'Not tested'}</td
              ><td
                ><a
                  class="button button-secondary"
                  href={resolve(`/providers/${item.id}`)}
                  >{canManage ? 'Manage' : 'View'}</a
                ></td
              ></tr
            >{/each}{/each}</tbody
      >
    </table>
  </div>
  <CursorPagination
    {...cursorPaginationProps(
      listState,
      providers.isPlaceholderData ? null : providers.data?.nextCursor,
      () => (selectedIds = [])
    )}
    label="Provider pages"
  />
{/if}

<style>
  /* Vendor group rows read as a band between the column headers and the
     connections they group; the heading itself keeps the mono caption. */
  .vendor-group th {
    border-bottom-color: var(--border-hairline);
    background: var(--surface-raised);
  }
  #provider-search {
    max-width: 30rem;
    display: block;
    margin: 0.5rem 0 1rem;
  }
  .updating {
    margin: 0 0 0.5rem;
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
  .connector-name {
    display: block;
    color: var(--foreground-muted);
  }
  .provider-table {
    margin-top: 1.5rem;
  }
  .updating + .provider-table {
    margin-top: 0;
  }
  .table-link {
    min-height: 2.75rem;
    color: var(--foreground);
    font-weight: 400;
    text-decoration: underline;
    text-decoration-color: var(--border-strong);
    text-underline-offset: 4px;
    transition:
      color var(--motion),
      text-decoration-color var(--motion);
  }
  .table-link:hover {
    color: var(--foreground-hover);
    text-decoration-color: currentColor;
  }
</style>
