<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import type { CodeAccount, CodePool } from '#lib/features/code-mode/api.js';
  import PoolEditor from '../PoolEditor.svelte';

  let {
    client,
    current,
    accounts
  }: {
    client: QueryClient;
    current: CodePool;
    accounts: CodeAccount[];
  } = $props();
</script>

<QueryClientProvider {client}>
  <PoolEditor
    {current}
    {accounts}
    projectId={current.project_id}
    allowed={true}
    onDraftChange={() => {}}
    onSaved={async () => {}}
    onCancel={() => {}}
  />
</QueryClientProvider>
