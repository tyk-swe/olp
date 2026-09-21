<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import ProjectsPanel from '../ProjectsPanel.svelte';

  let { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}>
  <ProjectsPanel />
</QueryClientProvider>
