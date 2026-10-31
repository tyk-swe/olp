<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import BillingImportSection from '../BillingImportSection.svelte';
  import type { Provider } from '../api/providers';

  const { client, provider }: { client: QueryClient; provider: Provider } =
    $props();
</script>

<QueryClientProvider {client}>
  <BillingImportSection {provider} canManage={true} />
</QueryClientProvider>
