<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import DiscountsPanel from '../DiscountsPanel.svelte';

  let { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}>
  <DiscountsPanel />
</QueryClientProvider>
