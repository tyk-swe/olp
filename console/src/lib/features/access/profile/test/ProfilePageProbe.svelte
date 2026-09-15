<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import ProfilePage from '../ProfilePage.svelte';

  let { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}><ProfilePage /></QueryClientProvider>
