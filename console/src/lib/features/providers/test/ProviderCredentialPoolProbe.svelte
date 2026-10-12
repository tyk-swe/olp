<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import ProviderCredentialPool from '../ProviderCredentialPool.svelte';
  import type { Provider } from '../api/providers';

  let {
    client,
    provider,
    onChanged
  }: {
    client: QueryClient;
    provider: Provider;
    onChanged: () => void | Promise<void>;
  } = $props();
</script>

<QueryClientProvider {client}>
  <ProviderCredentialPool {provider} canManage={true} {onChanged} />
</QueryClientProvider>
