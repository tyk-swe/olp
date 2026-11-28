<script lang="ts">
  import PageHeader from '#lib/components/PageHeader.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import QueryView from '#lib/components/QueryView.svelte';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import ProviderBulkActions from './ProviderBulkActions.svelte';
  import { providerKeys } from '#lib/domain/providers/providerKeys.js';

  import { overviewKeys } from '#lib/features/overview/index.js';

  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import NavIcon from '#lib/components/NavIcon.svelte';
  import CursorPagination from '#lib/components/CursorPagination.svelte';
  import ReadOnlyNote from '#lib/components/ReadOnlyNote.svelte';
  import { listProviderPage } from '#lib/domain/providers/api.js';
  import {
    cursorPaginationProps,
    resetCursor
  } from '#lib/listing/pagination.js';
  import { debouncedSearch } from '#lib/listing/search.svelte.js';
  import type { ProviderListState } from './providerPagination';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import { formatDate, stateLabel } from '#lib/format.js';
  import {
    providerStatus,
    providerStatusTone
  } from '#lib/features/providers/providerReadiness.js';

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
      queryKey: providerKeys.filteredPage(cursor, applied),
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

<PageHeader
  title="Providers"
  description="Connect upstream providers, review their models, and manage activation."
>
  {#snippet actions()}
    {#if canManage}<a
        class="button button-primary"
        href={resolve('providers/new')}
      >
        Add provider <NavIcon name="arrow" />
      </a>
    {/if}
  {/snippet}
</PageHeader>
{#if !canManage}
  <ReadOnlyNote>
    Your role can view providers but not connect, edit, or activate them.
  </ReadOnlyNote>
{/if}

<div class="toolbar" role="search">
  <label class="search">
    <span class="sr-only">Search connections</span><input
      id="provider-search"
      class="filter-control"
      type="search"
      bind:value={listState.search}
      oninput={() => {
        if (listState.search === '') search.applyNow();
        else search.schedule();
      }}
      placeholder="Name or vendor"
    />
  </label>
</div>

{#if canManage}
  <ProviderBulkActions
    {selected}
    onChanged={async () => {
      await providers.refetch();
      await queryClient.invalidateQueries({ queryKey: overviewKeys.root });
      selectedIds = [];
    }}
  />{/if}

<QueryView
  query={providers}
  loading="Loading providers…"
  failure={errorMessage(providers.error)}
  updating={providers.isPlaceholderData}
  emptyWhen={providers.data?.items.length === 0 &&
    listState.history.length === 0}
>
  {#snippet empty()}
    <EmptyState title="No providers configured">
      Connect an upstream and test it before building a route.
      {#snippet action()}
        {#if canManage}<a
            class="button button-primary"
            href={resolve('providers/new')}
          >
            Connect provider
          </a>
        {/if}
      {/snippet}
    </EmptyState>
  {/snippet}
  <div class="provider-table">
    <ScrollTable label="Providers" busy={providers.isPlaceholderData}>
      <table class="data-table">
        <thead>
          <tr>
            <th><span class="sr-only">Select</span></th><th>Name</th><th>
              Vendor / connector
            </th><th>Status</th><th>Models</th><th>Last probe</th><th>
              <span class="sr-only">Actions</span>
            </th>
          </tr>
        </thead><tbody>
          {#each groups as [vendor, connections] (vendor)}<tr
              class="vendor-group"
            >
              <th colspan="7" scope="rowgroup">
                {vendor} · {connections.length} connections on this page
              </th>
            </tr>{#each connections as item (item.id)}<tr>
                <td>
                  {#if canManage}<input
                      type="checkbox"
                      aria-label="Select {item.name}"
                      checked={selectedSet.has(item.id)}
                      disabled={providers.isPlaceholderData}
                      onchange={(e) => {
                        selectedIds = e.currentTarget.checked
                          ? [...selectedIds, item.id]
                          : selectedIds.filter((id) => id !== item.id);
                      }}
                    />
                  {/if}
                </td>
                <td>
                  <a class="table-link" href={resolve(`providers/${item.id}`)}>
                    {item.name}
                  </a>
                  <br />
                  <small>{item.project_name ?? 'Installation-wide'}</small>
                </td>
                <td>
                  {item.vendor_id ?? 'Custom'}
                  <small class="connector-name">{stateLabel(item.kind)}</small>
                </td>
                <td>
                  <span class="badge {providerStatusTone(item)}">
                    {providerStatus(item)}
                  </span>
                </td>
                <td>{item.enabled_model_count} enabled</td>
                <td>
                  {item.last_probe_at
                    ? formatDate(item.last_probe_at)
                    : 'Not tested'}
                </td>
                <td>
                  <a
                    class="button button-secondary"
                    href={resolve(`providers/${item.id}`)}
                  >
                    {canManage ? 'Manage' : 'View'}
                  </a>
                </td>
              </tr>
            {/each}
          {/each}
        </tbody>
      </table>
    </ScrollTable>
  </div>
  <CursorPagination
    {...cursorPaginationProps(
      listState,
      providers.isPlaceholderData ? null : providers.data?.nextCursor,
      () => (selectedIds = [])
    )}
    label="Provider pages"
  />
</QueryView>

<style>
  /* Vendor group rows read as a band between the column headers and the
     connections they group; the heading itself keeps the mono caption. */
  .vendor-group th {
    border-bottom-color: var(--border-hairline);
    background: var(--surface-raised);
  }
  .search {
    flex: 1 1 auto;
    max-width: 30rem;
  }
  .search input {
    width: 100%;
  }
  .connector-name {
    display: block;
    color: var(--foreground-muted);
  }
  .provider-table {
    margin-top: 0.75rem;
  }
  .provider-table :global(.scroll-table) {
    margin-top: 0;
  }
</style>
