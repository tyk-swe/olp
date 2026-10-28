<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import GrantsPage from '../GrantsPage.svelte';
  const { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}><GrantsPage /></QueryClientProvider>
