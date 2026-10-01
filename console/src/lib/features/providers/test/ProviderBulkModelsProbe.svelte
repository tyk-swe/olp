<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import ProviderBulkModels from '../ProviderBulkModels.svelte';
  import type { Provider } from '../api/providers';

  let { client, provider }: { client: QueryClient; provider: Provider } =
    $props();
</script>

<QueryClientProvider {client}>
  <ProviderBulkModels {provider} canManage onChanged={() => {}} />
</QueryClientProvider>
