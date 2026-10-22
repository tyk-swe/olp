<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import type { ComponentProps } from 'svelte';
  import type { CodeRoute } from '$lib/api/code-mode';
  import ClientConfiguration from '../ClientConfiguration.svelte';
  import CodeManagement from '../CodeManagement.svelte';

  let {
    client,
    route,
    load
  }: {
    client: QueryClient;
    route: CodeRoute;
    load?: ComponentProps<typeof ClientConfiguration>['load'];
  } = $props();
</script>

<QueryClientProvider {client}>
  {#if load}
    <ClientConfiguration {route} gatewayURL="https://olp.example" {load} />
  {:else}
    <CodeManagement
      projectId={route.project_id}
      kind="routes"
      allowed={true}
      gatewayURL="https://olp.example"
      onDraftChange={() => {}}
    />
  {/if}
</QueryClientProvider>
