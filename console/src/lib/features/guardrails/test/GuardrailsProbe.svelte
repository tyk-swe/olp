<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import GuardrailsPage from '../GuardrailsPage.svelte';
  let { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}><GuardrailsPage /></QueryClientProvider>
