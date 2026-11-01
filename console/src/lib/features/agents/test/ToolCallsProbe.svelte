<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import ToolCallsPage from '../ToolCallsPage.svelte';
  let { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}>
  <ToolCallsPage />
</QueryClientProvider>
