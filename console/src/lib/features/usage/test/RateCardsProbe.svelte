<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import RateCardsPanel from '../RateCardsPanel.svelte';

  let { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}>
  <RateCardsPanel />
</QueryClientProvider>
