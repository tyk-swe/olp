<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import PricingSourcesPanel from '../PricingSourcesPanel.svelte';

  let { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}>
  <PricingSourcesPanel />
</QueryClientProvider>
