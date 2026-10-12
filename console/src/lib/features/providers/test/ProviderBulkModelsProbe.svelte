<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import ProviderBulkModels from '../ProviderBulkModels.svelte';
  import type { Provider } from '../api/providers';

  let {
    client,
    provider,
    onChanged = () => {}
  }: {
    client: QueryClient;
    provider: Provider;
    onChanged?: () => void | Promise<void>;
  } = $props();
  export function replaceProvider(next: Provider) {
    provider = next;
  }
</script>

<QueryClientProvider {client}>
  <ProviderBulkModels {provider} canManage {onChanged} />
</QueryClientProvider>
