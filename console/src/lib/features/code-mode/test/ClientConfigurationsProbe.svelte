<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import type { ComponentProps } from 'svelte';
  import type { CodeRoute } from '$lib/api/code-mode';
  import ClientConfiguration from '../ClientConfiguration.svelte';

  let {
    client,
    routes,
    gatewayURL,
    load
  }: {
    client: QueryClient;
    routes: CodeRoute[];
    gatewayURL: string;
    load: ComponentProps<typeof ClientConfiguration>['load'];
  } = $props();
</script>

<QueryClientProvider {client}>
  {#each routes as route (route.id)}
    <ClientConfiguration {route} {gatewayURL} {load} />
  {/each}
</QueryClientProvider>
