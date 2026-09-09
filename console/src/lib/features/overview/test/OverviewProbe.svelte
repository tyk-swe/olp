<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import Overview from '../Overview.svelte';
  let { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}><Overview /></QueryClientProvider>
