<script lang="ts">
  import { resolve } from '$app/paths';
  import { queryKeys } from '$lib/api/queryKeys';
  import { createQuery } from '@tanstack/svelte-query';
  import { errorMessage as message } from '$lib/api/http';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import { listProviderPage } from '$lib/api/management/providers';
  import {
    cursorPaginationProps,
    type CursorHistory
  } from '$lib/api/pagination';
  import { useRole } from '$lib/auth/useRole.svelte';
  import { formatDate, stateLabel } from '$lib/format';
  import { providerStatus, providerStatusTone } from './providerEditor';

  let { listState = $bindable() }: { listState: CursorHistory } = $props();
  const access = useRole();
  const canManage = $derived(access.can('providers.manage'));
  const providers = createQuery(() => ({
    queryKey: queryKeys.providers.page(listState.cursor),
    queryFn: ({ signal }) => listProviderPage(listState.cursor, signal)
  }));
</script>

<div class="page-header">
  <div>
    <p class="eyebrow">Gateway</p>
    <h1 class="page-title">Providers</h1>
    <p class="page-description">
      Each named provider has one active credential version and explicit
      certified capabilities.
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
  <div class="table-shell provider-table">
    <table class="data-table">
      <thead
        ><tr
          ><th>Name</th><th>Connector</th><th>Status</th><th>Models</th><th
            >Last probe</th
          ><th><span class="sr-only">Actions</span></th></tr
        ></thead
      ><tbody
        >{#each providers.data?.items ?? [] as item (item.id)}<tr
            ><td
              ><a class="table-link" href={resolve(`/providers/${item.id}`)}
                >{item.name}</a
              ></td
            ><td>{stateLabel(item.kind)}</td><td
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
          >{/each}</tbody
      >
    </table>
  </div>
  <CursorPagination
    {...cursorPaginationProps(listState, providers.data?.nextCursor)}
    label="Provider pages"
  />
{/if}

<style>
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
