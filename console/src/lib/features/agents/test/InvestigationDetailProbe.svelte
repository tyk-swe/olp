<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import InvestigationDetailPage from '../InvestigationDetailPage.svelte';
  let { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}>
  <InvestigationDetailPage investigationID="investigation" />
</QueryClientProvider>
