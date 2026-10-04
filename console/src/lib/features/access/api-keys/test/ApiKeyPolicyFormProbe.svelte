<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import type { ApiKey } from '../api';
  import ApiKeyPolicyForm from '../ApiKeyPolicyForm.svelte';
  import type { ApiKeyPolicyInput } from '../apiKeyPolicy';

  let {
    client,
    editing,
    canManage = true,
    onSubmit
  }: {
    client: QueryClient;
    editing: ApiKey;
    canManage?: boolean;
    onSubmit: (input: ApiKeyPolicyInput) => boolean;
  } = $props();
</script>

<QueryClientProvider {client}>
  <ApiKeyPolicyForm
    {editing}
    {canManage}
    busy=""
    submitError=""
    {onSubmit}
    onCancel={() => {}}
    onClearError={() => {}}
  />
</QueryClientProvider>
