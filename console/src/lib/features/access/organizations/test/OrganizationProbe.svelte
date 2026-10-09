<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import OrganizationsPage from '../OrganizationsPage.svelte';
  let { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}><OrganizationsPage /></QueryClientProvider>
