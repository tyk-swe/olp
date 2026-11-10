<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import AttemptsDiagnostics from '../AttemptsDiagnostics.svelte';

  let { client, projectId }: { client: QueryClient; projectId: string } =
    $props();
</script>

<QueryClientProvider {client}>
  <AttemptsDiagnostics {projectId} />
</QueryClientProvider>
