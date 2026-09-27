<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import UnconfinedPlugins from '../UnconfinedPlugins.svelte';

  let {
    client,
    enabled,
    canManage,
    onPermitted
  }: {
    client: QueryClient;
    enabled: boolean;
    canManage: boolean;
    onPermitted: () => Promise<unknown>;
  } = $props();
</script>

<QueryClientProvider {client}>
  <UnconfinedPlugins {enabled} {canManage} {onPermitted} />
</QueryClientProvider>
