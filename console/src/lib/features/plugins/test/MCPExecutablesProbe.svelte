<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import MCPExecutables from '../MCPExecutables.svelte';
  const { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}
  ><MCPExecutables enabled={true} canManage={true} /></QueryClientProvider
>
