<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import RouteDraftEditor from '../RouteDraftEditor.svelte';
  const { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}
  ><RouteDraftEditor routeId="route" /></QueryClientProvider
>
