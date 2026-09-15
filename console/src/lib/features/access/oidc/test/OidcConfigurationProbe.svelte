<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import OidcConfigurationPanel from '../OidcConfigurationPanel.svelte';

  let { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}><OidcConfigurationPanel /></QueryClientProvider>
