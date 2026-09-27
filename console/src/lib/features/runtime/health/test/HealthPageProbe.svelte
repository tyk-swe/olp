<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import HealthPage from '../HealthPage.svelte';

  let { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}><HealthPage /></QueryClientProvider>
