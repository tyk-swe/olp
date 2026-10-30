<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import CodeDiagnostics from '../CodeDiagnostics.svelte';

  let { client, projectId }: { client: QueryClient; projectId: string } =
    $props();
</script>

<QueryClientProvider {client}>
  <CodeDiagnostics {projectId} kind="attempts" allowed={false} />
</QueryClientProvider>
