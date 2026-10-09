<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import WorkloadIssuersPage from '../WorkloadIssuersPage.svelte';
  let { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}><WorkloadIssuersPage /></QueryClientProvider>
