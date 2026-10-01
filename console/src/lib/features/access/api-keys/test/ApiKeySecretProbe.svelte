<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import type { ApiKeySecret } from '../api';
  import ApiKeySecretDialog from '../ApiKeySecretDialog.svelte';

  let { client, secret }: { client: QueryClient; secret: ApiKeySecret } =
    $props();
</script>

<QueryClientProvider {client}>
  <ApiKeySecretDialog
    {secret}
    context="created"
    preferredRoute="chat"
    onClose={() => {}}
  />
</QueryClientProvider>
