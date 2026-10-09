<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import ScimPage from '../ScimPage.svelte';
  let { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}><ScimPage /></QueryClientProvider>
