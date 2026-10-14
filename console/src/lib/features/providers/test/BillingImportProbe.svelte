<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import BillingImportSection from '../BillingImportSection.svelte';
  import type { Provider } from '../api/providers';

  let {
    client,
    provider,
    canManage = true
  }: {
    client: QueryClient;
    provider: Provider;
    canManage?: boolean;
  } = $props();
</script>

<QueryClientProvider {client}>
  <BillingImportSection {provider} {canManage} />
</QueryClientProvider>
