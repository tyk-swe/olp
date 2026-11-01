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
    providers = [],
    allowed = true
  }: {
    client: QueryClient;
    current: CodeRoute;
    providers?: PaidTargetOption[];
    allowed?: boolean;
  } = $props();
</script>

<QueryClientProvider {client}>
  <RouteEditor
    {current}
    {providers}
    accounts={[]}
    pools={[]}
    projectId={current.project_id}
    {allowed}
    onDraftChange={() => {}}
    onSaved={async () => {}}
    onCancel={() => {}}
  />
</QueryClientProvider>
