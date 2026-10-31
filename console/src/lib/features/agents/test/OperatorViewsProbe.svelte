<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import TaskDetailPage from '../TaskDetailPage.svelte';
  import DelegationsPage from '../DelegationsPage.svelte';
  let { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}>
  <TaskDetailPage taskID="task" />
  <DelegationsPage />
</QueryClientProvider>
