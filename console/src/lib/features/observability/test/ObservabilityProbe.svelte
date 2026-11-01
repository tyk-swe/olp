<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import ObservabilityPanel from '../ObservabilityPanel.svelte';
  const { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}><ObservabilityPanel /></QueryClientProvider>
