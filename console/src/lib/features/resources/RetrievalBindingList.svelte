<script lang="ts">
  import QueryView from '#lib/components/QueryView.svelte';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import type { CreateQueryResult } from '@tanstack/svelte-query';
  import type { RetrievalBinding } from './api';

  const {
    bindings,
    busy,
    edit
  }: {
    bindings: CreateQueryResult<RetrievalBinding[]>;
    busy: boolean;
    edit: (id?: string) => void;
  } = $props();
</script>

<QueryView
  query={bindings}
  loading="Loading retrieval bindings…"
  emptyWhen={!bindings.data?.length}
>
  {#snippet empty()}
    <p>No cloud retrieval bindings are configured.</p>
  {/snippet}
  <ScrollTable label="Cloud retrieval bindings">
    <table class="data-table">
      <caption class="sr-only">Cloud retrieval bindings</caption>
      <thead>
        <tr>
          <th scope="col">Name</th><th scope="col">Backend</th><th scope="col">
            Route
          </th><th scope="col">Access</th><th scope="col">Actions</th>
        </tr>
      </thead>
      <tbody>
        {#each bindings.data ?? [] as item (item.id)}<tr>
            <td>{item.name}<small class="mono">{item.id}</small></td><td>
              {item.backend}<small>{item.backend_resource}</small>
            </td>
            <td>{item.route}</td><td>
              {item.enabled ? 'Enabled' : 'Disabled'} · {item.key_grants.length} grants
            </td>
            <td>
              <button
                class="text-button"
                type="button"
                onclick={() => edit(item.id)}
                disabled={busy}
              >
                Inspect / edit
              </button>
            </td>
          </tr>{/each}
      </tbody>
    </table>
  </ScrollTable>
</QueryView>

<style>
  td small {
    display: block;
    color: var(--foreground-muted);
    overflow-wrap: anywhere;
  }
</style>
