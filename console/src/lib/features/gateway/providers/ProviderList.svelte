<script lang="ts">
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { errorMessage as message } from '$lib/api/http';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import { listProviderPage } from '$lib/api/management/providers';
  import {
    popCursor,
    pushCursor,
    type CursorHistory
  } from '$lib/api/pagination';
  import { providerStatus } from './providerEditor';

  let { listState = $bindable() }: { listState: CursorHistory } = $props();
  const providers = createQuery(() => ({
    queryKey: ['provider-page', listState.cursor ?? 'first'],
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
  <a class="button button-primary" href={resolve('/providers/new')}
    >Add provider <NavIcon name="arrow" /></a
  >
</div>

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
      <a class="button button-primary" href={resolve('/providers/new')}
        >Connect provider</a
      >
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
            ><td>{item.kind.replaceAll('_', ' ')}</td><td
              ><span
                class:success={item.active_revision != null &&
                  !item.pending_activation}
                class:warning={item.pending_activation ||
                  item.state === 'draft'}
                class="badge">{providerStatus(item)}</span
              ></td
            ><td>{item.enabled_model_count} enabled</td><td
              >{item.last_probe_at
                ? new Date(item.last_probe_at).toLocaleString()
                : 'Not tested'}</td
            ><td
              ><a
                class="button button-secondary"
                href={resolve(`/providers/${item.id}`)}>Manage</a
              ></td
            ></tr
          >{/each}</tbody
      >
    </table>
  </div>
  <CursorPagination
    page={listState.history.length + 1}
    hasPrevious={listState.history.length > 0}
    hasNext={Boolean(providers.data?.nextCursor)}
    onPrevious={() => popCursor(listState)}
    onNext={() => pushCursor(listState, providers.data?.nextCursor)}
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
