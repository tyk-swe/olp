<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import BudgetGroupsPanel from '../BudgetGroupsPanel.svelte';
  let { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}>
  <BudgetGroupsPanel />
</QueryClientProvider>
