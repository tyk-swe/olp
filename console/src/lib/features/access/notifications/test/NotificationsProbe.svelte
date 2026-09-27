<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import NotificationsPanel from '../NotificationsPanel.svelte';

  let { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}>
  <NotificationsPanel />
</QueryClientProvider>
