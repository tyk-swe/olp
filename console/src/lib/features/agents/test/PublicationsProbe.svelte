<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import PublicationsPage from '../PublicationsPage.svelte';
  const { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}><PublicationsPage /></QueryClientProvider>
