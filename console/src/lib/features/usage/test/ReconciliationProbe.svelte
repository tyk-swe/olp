<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import ReconciliationPanel from '../ReconciliationPanel.svelte';

  const { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}>
  <ReconciliationPanel
    start="2026-01-01T00:00:00Z"
    end="2026-01-03T00:00:00Z"
  />
</QueryClientProvider>
