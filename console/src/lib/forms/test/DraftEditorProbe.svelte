<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import ProviderDetail from '$lib/features/providers/ProviderDetail.svelte';
  import ProviderWizard from '$lib/features/providers/ProviderWizard.svelte';
  import RouteDraftEditor from '$lib/features/routes/RouteDraftEditor.svelte';
  import RouteRevisionHistory from '$lib/features/routes/RouteRevisionHistory.svelte';
  import ApiKeysPage from '$lib/features/access/api-keys/ApiKeysPage.svelte';
  import { apiKeyList } from '$lib/features/access/api-keys/apiKeyListState';

  let {
    client,
    kind
  }: {
    client: QueryClient;
    kind: 'route' | 'provider' | 'key' | 'history' | 'wizard';
  } = $props();
  let listState = $state(apiKeyList.empty());
</script>

<QueryClientProvider {client}>
  {#if kind === 'route'}
    <RouteDraftEditor routeId="route-a" />
  {:else if kind === 'provider'}
    <ProviderDetail providerId="provider-a" />
  {:else if kind === 'history'}
    <RouteRevisionHistory routeId="route-a" />
  {:else if kind === 'wizard'}
    <ProviderWizard />
  {:else}
    <ApiKeysPage bind:listState />
  {/if}
</QueryClientProvider>
