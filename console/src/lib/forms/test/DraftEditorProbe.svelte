<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import ProviderDetail from '$lib/features/providers/ProviderDetail.svelte';
  import RouteDraftEditor from '$lib/features/routes/RouteDraftEditor.svelte';

  let { client, kind }: { client: QueryClient; kind: 'route' | 'provider' } =
    $props();
</script>

<QueryClientProvider {client}>
  {#if kind === 'route'}
    <RouteDraftEditor routeId="route-a" />
  {:else}
    <ProviderDetail providerId="provider-a" />
  {/if}
</QueryClientProvider>
