<script lang="ts">
  import ProviderBulkActions from './ProviderBulkActions.svelte';
  import { providerKeys } from '$lib/features/providers/providerKeys';

  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { errorMessage as message } from '$lib/api/http';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import { listProviderPage } from '$lib/features/providers/api';
  import { cursorPaginationProps, resetCursor } from '$lib/lists/pagination';
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
  let selectedIds = $state<string[]>([]);
  const selectedSet = $derived(new Set(selectedIds));
  const providers = createQuery(() => ({
    queryKey: [...providerKeys.page(listState.cursor), listState.search],
    queryFn: ({ signal }) =>
      listProviderPage(listState.cursor, signal, listState.search)
  }));
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
    resetCursor(listState);
    selectedIds = [];
  }}
  placeholder="Name or vendor"
  type="search"
/>
{#if canManage}<ProviderBulkActions
    {selected}
    onChanged={async () => {
      await providers.refetch();
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
  <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
  <div
    class="table-shell provider-table"
    tabindex="0"
    role="region"
    aria-label="Providers"
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
                    onchange={(e) => {
                      selectedIds = e.currentTarget.checked
                        ? [...selectedIds, item.id]
                        : selectedIds.filter((id) => id !== item.id);
                    }}
                  />{/if}</td
              ><td
                ><a class="table-link" href={resolve(`/providers/${item.id}`)}
                  >{item.name}</a
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
    {...cursorPaginationProps(listState, providers.data?.nextCursor)}
    label="Provider pages"
  />
{/if}

<style>
  .vendor-group th {
    background: var(--surface-subtle);
    text-transform: none;
    padding: 0.85rem 1rem;
  }
  #provider-search {
    max-width: 30rem;
    display: block;
    margin: 0.5rem 0 1rem;
  }
  .connector-name {
    display: block;
    color: var(--foreground-muted);
  }
  h2 {
    margin: 0 0 0.85rem;
    font-size: 1.15rem;
    font-weight: 750;
    letter-spacing: -0.025em;
  }
  .provider-table {
    margin-top: 1.5rem;
  }
  .table-link {
    min-height: 2.75rem;
    color: var(--accent-strong);
    font-weight: 750;
    text-underline-offset: 0.18rem;
  }
</style>
