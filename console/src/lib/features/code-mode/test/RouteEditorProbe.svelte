<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import type { CodeRoute } from '$lib/api/code-mode';
  import RouteEditor from '../RouteEditor.svelte';

  let { client, current }: { client: QueryClient; current: CodeRoute } =
    $props();
</script>

<QueryClientProvider {client}>
  <RouteEditor
    {current}
    accounts={[]}
    pools={[]}
    projectId={current.project_id}
    allowed={true}
    onDraftChange={() => {}}
    onSaved={async () => {}}
    onCancel={() => {}}
  />
</QueryClientProvider>
