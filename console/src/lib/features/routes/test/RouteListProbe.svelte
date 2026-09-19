<script lang="ts">
  import { untrack } from 'svelte';
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import RouteList from '../RouteList.svelte';
  import type { RouteListState } from '../routeListState';

  let {
    client,
    initialState
  }: { client: QueryClient; initialState: RouteListState } = $props();
  let listState = $state(untrack(() => initialState));
  let visible = $state(true);
</script>

<QueryClientProvider {client}>
  <button type="button" onclick={() => (visible = !visible)}>
    {visible ? 'Open detail' : 'Return to list'}
  </button>
  {#if visible}<RouteList bind:listState />{/if}
</QueryClientProvider>
