<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import BudgetIncreasesPage from '../BudgetIncreasesPage.svelte';
  let { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}><BudgetIncreasesPage /></QueryClientProvider>
