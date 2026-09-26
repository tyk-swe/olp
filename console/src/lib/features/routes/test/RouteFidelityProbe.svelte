<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import RouteDraftEditor from '../RouteDraftEditor.svelte';
  import RouteRevisionHistory from '../RouteRevisionHistory.svelte';

  let {
    client,
    view,
    routeId
  }: {
    client: QueryClient;
    view: 'editor' | 'history';
    routeId?: string;
  } = $props();
</script>

<QueryClientProvider {client}>
  {#if view === 'editor'}
    <RouteDraftEditor {routeId} />
  {:else}
    <RouteRevisionHistory routeId={routeId ?? 'route-a'} />
  {/if}
</QueryClientProvider>
