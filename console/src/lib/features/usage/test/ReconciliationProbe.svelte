<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import ReconciliationPanel from '../ReconciliationPanel.svelte';

  let {
    client,
    start,
    end
  }: { client: QueryClient; start: string; end: string } = $props();
</script>

<QueryClientProvider {client}>
  <ReconciliationPanel {start} {end} />
</QueryClientProvider>
