<script lang="ts">
  import { untrack } from 'svelte';
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import ProviderList from '../ProviderList.svelte';
  import type { ProviderListState } from '../providerPagination';

  let {
    client,
    initialState
  }: { client: QueryClient; initialState: ProviderListState } = $props();
  let listState = $state(untrack(() => initialState));
  let visible = $state(true);
</script>

<QueryClientProvider {client}>
  <button type="button" onclick={() => (visible = !visible)}>
    {visible ? 'Open detail' : 'Return to list'}
  </button>
  {#if visible}<ProviderList bind:listState />{/if}
</QueryClientProvider>
