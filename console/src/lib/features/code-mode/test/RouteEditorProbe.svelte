<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import type { CodeRoute } from '$lib/api/code-mode';
  import RouteEditor from '../RouteEditor.svelte';
  import type { PaidTargetOption } from '../presentation';

  let {
    client,
    current,
    providers = []
  }: {
    client: QueryClient;
    current: CodeRoute;
    providers?: PaidTargetOption[];
  } = $props();
</script>

<QueryClientProvider {client}>
  <RouteEditor
    {current}
    {providers}
    accounts={[]}
    pools={[]}
    projectId={current.project_id}
    allowed={true}
    onDraftChange={() => {}}
    onSaved={async () => {}}
    onCancel={() => {}}
  />
</QueryClientProvider>
